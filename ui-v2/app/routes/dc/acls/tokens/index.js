import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import { hash } from 'rsvp';
import { get } from '@ember/object';
import WithTokenActions from 'consul-ui/mixins/token/with-actions';
export default Route.extend(WithTokenActions, {
  repo: service('repository/token'),
  settings: service('settings'),
  queryParams: {
    sortBy: 'sort',
    search: {
      as: 'filter',
      replace: true,
    },
  },
  beforeModel: function(transition) {
    return this.settings.findBySlug('token').then(token => {
      // If you have a token set with AccessorID set to null (legacy mode)
      // then rewrite to the old acls
      if (token && get(token, 'AccessorID') === null) {
        // If you return here, you get a TransitionAborted error in the tests only
        // everything works fine either way checking things manually
        this.replaceWith('dc.acls');
      }
    });
  },
  model: function(params) {
    return hash({
      ...this.repo.status({
        items: this.repo.findAllByDatacenter(
          this.modelFor('dc').dc.Name,
          this.modelFor('nspace').nspace.substr(1)
        ),
      }),
      nspace: this.modelFor('nspace').nspace.substr(1),
      isLoading: false,
      token: this.settings.findBySlug('token'),
    });
  },
  setupController: function(controller, model) {
    controller.setProperties(model);
  },
});
