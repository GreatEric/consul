package autoconf

import (
	"context"
	"fmt"
	"net"

	"github.com/hashicorp/consul/agent/cache"
	cachetype "github.com/hashicorp/consul/agent/cache-types"
	"github.com/hashicorp/consul/agent/connect"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/proto/pbautoconf"
)

const (
	// ID of the roots watch
	rootsWatchID = "roots"

	// ID of the leaf watch
	leafWatchID = "leaf"
)

func extractPEMs(roots *structs.IndexedCARoots) []string {
	var pems []string
	for _, root := range roots.Roots {
		pems = append(pems, root.RootCert)
	}
	return pems
}

// updateTLSFromResponse will update the TLS certificate and roots in the shared
// TLS configurator.
func (ac *AutoConfig) updateTLSFromResponse(resp *pbautoconf.AutoConfigResponse) error {
	roots, err := translateCARootsToStructs(resp.CARoots)
	if err != nil {
		return err
	}

	cert, err := translateIssuedCertToStructs(resp.Certificate)
	if err != nil {
		return err
	}

	update := &structs.SignedResponse{
		IssuedCert:     *cert,
		ConnectCARoots: *roots,
		ManualCARoots:  resp.ExtraCACertificates,
	}

	if resp.Config != nil && resp.Config.TLS != nil {
		update.VerifyServerHostname = resp.Config.TLS.VerifyServerHostname
	}

	return ac.setInitialTLSCertificates(update)
}

func (ac *AutoConfig) setInitialTLSCertificates(certs *structs.SignedResponse) error {
	if certs == nil {
		return nil
	}

	if err := ac.populateCertificateCache(certs); err != nil {
		return fmt.Errorf("error populating cache with certificates: %w", err)
	}

	connectCAPems := extractPEMs(&certs.ConnectCARoots)

	err := ac.acConfig.TLSConfigurator.UpdateAutoTLS(
		certs.ManualCARoots,
		connectCAPems,
		certs.IssuedCert.CertPEM,
		certs.IssuedCert.PrivateKeyPEM,
		certs.VerifyServerHostname,
	)

	if err != nil {
		return fmt.Errorf("error updating TLS configurator with certificates: %w", err)
	}

	return nil
}

func (ac *AutoConfig) populateCertificateCache(certs *structs.SignedResponse) error {
	cert, err := connect.ParseCert(certs.IssuedCert.CertPEM)
	if err != nil {
		return fmt.Errorf("Failed to parse certificate: %w", err)
	}

	// prepolutate roots cache
	rootRes := cache.FetchResult{Value: &certs.ConnectCARoots, Index: certs.ConnectCARoots.QueryMeta.Index}
	rootsReq := ac.caRootsRequest()
	// getting the roots doesn't require a token so in order to potentially share the cache with another
	if err := ac.acConfig.Cache.Prepopulate(cachetype.ConnectCARootName, rootRes, ac.config.Datacenter, "", rootsReq.CacheInfo().Key); err != nil {
		return err
	}

	leafReq := ac.leafCertRequest()

	// prepolutate leaf cache
	certRes := cache.FetchResult{
		Value: &certs.IssuedCert,
		Index: certs.ConnectCARoots.QueryMeta.Index,
		State: cachetype.ConnectCALeafSuccess(connect.EncodeSigningKeyID(cert.AuthorityKeyId)),
	}
	if err := ac.acConfig.Cache.Prepopulate(cachetype.ConnectCALeafName, certRes, leafReq.Datacenter, leafReq.Token, leafReq.Key()); err != nil {
		return err
	}
	return nil
}

func (ac *AutoConfig) setupCertificateCacheWatches(ctx context.Context) (context.CancelFunc, error) {
	notificationCtx, cancel := context.WithCancel(ctx)

	rootsReq := ac.caRootsRequest()
	err := ac.acConfig.Cache.Notify(notificationCtx, cachetype.ConnectCARootName, &rootsReq, rootsWatchID, ac.cacheUpdates)
	if err != nil {
		cancel()
		return nil, err
	}

	leafReq := ac.leafCertRequest()
	err = ac.acConfig.Cache.Notify(notificationCtx, cachetype.ConnectCALeafName, &leafReq, leafWatchID, ac.cacheUpdates)
	if err != nil {
		cancel()
		return nil, err
	}

	return cancel, nil
}

func (ac *AutoConfig) updateCARoots(roots *structs.IndexedCARoots) error {
	switch {
	case ac.config.AutoConfig.Enabled:
		var err error
		ac.autoConfigResponse.CARoots, err = translateCARootsToProtobuf(roots)
		if err != nil {
			return err
		}

		return ac.recordResponse(ac.autoConfigResponse)
	case ac.config.AutoEncryptTLS:
		pems := extractPEMs(roots)

		if err := ac.acConfig.TLSConfigurator.UpdateAutoTLSCA(pems); err != nil {
			return fmt.Errorf("failed to update Connect CA certificates: %w", err)
		}
		return nil
	default:
		return nil
	}
}

func (ac *AutoConfig) updateLeafCert(cert *structs.IssuedCert) error {
	switch {
	case ac.config.AutoConfig.Enabled:
		var err error
		ac.autoConfigResponse.Certificate, err = translateIssuedCertToProtobuf(cert)
		if err != nil {
			return err
		}
		return ac.recordResponse(ac.autoConfigResponse)
	case ac.config.AutoEncryptTLS:
		if err := ac.acConfig.TLSConfigurator.UpdateAutoTLSCert(cert.CertPEM, cert.PrivateKeyPEM); err != nil {
			return fmt.Errorf("failed to update the agent leaf cert: %w", err)
		}
		return nil
	default:
		return nil
	}
}

func (ac *AutoConfig) caRootsRequest() structs.DCSpecificRequest {
	return structs.DCSpecificRequest{Datacenter: ac.config.Datacenter}
}

func (ac *AutoConfig) leafCertRequest() cachetype.ConnectCALeafRequest {
	return cachetype.ConnectCALeafRequest{
		Datacenter: ac.config.Datacenter,
		Agent:      ac.config.NodeName,
		DNSSAN:     ac.getDNSSANs(),
		IPSAN:      ac.getIPSANs(),
		Token:      ac.acConfig.Tokens.AgentToken(),
	}
}

// generateCSR will generate a CSR for an Agent certificate. This should
// be sent along with the AutoConfig.InitialConfiguration RPC or the
// AutoEncrypt.Sign RPC. The generated CSR does NOT have a real trust domain
// as when generating this we do not yet have the CA roots. The server will
// update the trust domain for us though.
func (ac *AutoConfig) generateCSR() (csr string, key string, err error) {
	// We don't provide the correct host here, because we don't know any
	// better at this point. Apart from the domain, we would need the
	// ClusterID, which we don't have. This is why we go with
	// dummyTrustDomain the first time. Subsequent CSRs will have the
	// correct TrustDomain.
	id := &connect.SpiffeIDAgent{
		// will be replaced
		Host:       dummyTrustDomain,
		Datacenter: ac.config.Datacenter,
		Agent:      ac.config.NodeName,
	}

	caConfig, err := ac.config.ConnectCAConfiguration()
	if err != nil {
		return "", "", fmt.Errorf("Cannot generate CSR: %w", err)
	}

	conf, err := caConfig.GetCommonConfig()
	if err != nil {
		return "", "", fmt.Errorf("Failed to load common CA configuration: %w", err)
	}

	if conf.PrivateKeyType == "" {
		conf.PrivateKeyType = connect.DefaultPrivateKeyType
	}
	if conf.PrivateKeyBits == 0 {
		conf.PrivateKeyBits = connect.DefaultPrivateKeyBits
	}

	// Create a new private key
	pk, pkPEM, err := connect.GeneratePrivateKeyWithConfig(conf.PrivateKeyType, conf.PrivateKeyBits)
	if err != nil {
		return "", "", fmt.Errorf("Failed to generate private key: %w", err)
	}

	dnsNames := append([]string{"localhost"}, ac.getDNSSANs()...)
	ipAddresses := append([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::")}, ac.getIPSANs()...)

	// Create a CSR.
	//
	// The Common Name includes the dummy trust domain for now but Server will
	// override this when it is signed anyway so it's OK.
	cn := connect.AgentCN(ac.config.NodeName, dummyTrustDomain)
	csr, err = connect.CreateCSR(id, cn, pk, dnsNames, ipAddresses)
	if err != nil {
		return "", "", err
	}

	return csr, pkPEM, nil
}

func (ac *AutoConfig) getDNSSANs() []string {
	switch {
	case ac.config.AutoConfig.Enabled:
		return ac.config.AutoConfig.DNSSANs
	case ac.config.AutoEncryptTLS:
		return ac.config.AutoEncryptDNSSAN
	default:
		return nil
	}
}

func (ac *AutoConfig) getIPSANs() []net.IP {
	switch {
	case ac.config.AutoConfig.Enabled:
		return ac.config.AutoConfig.IPSANs
	case ac.config.AutoEncryptTLS:
		return ac.config.AutoEncryptIPSAN
	default:
		return nil
	}
}
