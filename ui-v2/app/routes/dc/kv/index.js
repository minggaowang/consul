import Route from 'consul-ui/routing/route';
import { inject as service } from '@ember/service';
import { hash } from 'rsvp';
import { get } from '@ember/object';
import isFolder from 'consul-ui/utils/isFolder';

export default Route.extend({
  queryParams: {
    search: {
      as: 'filter',
      replace: true,
    },
  },
  repo: service('repository/kv'),
  beforeModel: function() {
    // we are index or folder, so if the key doesn't have a trailing slash
    // add one to force a fake findBySlug
    const params = this.paramsFor(this.routeName);
    const key = params.key || '/';
    if (!isFolder(key)) {
      return this.replaceWith(this.routeName, key + '/');
    }
  },
  model: function(params) {
    let key = params.key || '/';
    const dc = this.modelFor('dc').dc.Name;
    const nspace = this.modelFor('nspace').nspace.substr(1);
    return hash({
      parent: this.repo.findBySlug(key, dc, nspace),
    }).then(model => {
      return hash({
        ...model,
        ...{
          items: this.repo.findAllBySlug(get(model.parent, 'Key'), dc, nspace).catch(e => {
            const status = get(e, 'errors.firstObject.status');
            switch (status) {
              case '403':
                return this.transitionTo('dc.acls.tokens');
              default:
                return this.transitionTo('dc.kv.index');
            }
          }),
        },
      });
    });
  },
  actions: {
    error: function(e) {
      if (e.errors && e.errors[0] && e.errors[0].status == '404') {
        return this.transitionTo('dc.kv.index');
      }
      throw e;
    },
  },
  setupController: function(controller, model) {
    this._super(...arguments);
    controller.setProperties(model);
  },
});
