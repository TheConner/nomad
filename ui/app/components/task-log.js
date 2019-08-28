import { inject as service } from '@ember/service';
import Component from '@ember/component';
import { computed } from '@ember/object';
import RSVP from 'rsvp';
import { logger } from 'nomad-ui/utils/classes/log';
import timeout from 'nomad-ui/utils/timeout';

export default Component.extend({
  token: service(),

  classNames: ['boxed-section', 'task-log'],

  allocation: null,
  task: null,

  // When true, request logs from the server agent
  useServer: false,

  // When true, logs cannot be fetched from either the client or the server
  noConnection: false,

  clientTimeout: 1000,
  serverTimeout: 5000,

  isStreaming: true,
  streamMode: 'streaming',

  mode: 'stdout',

  logUrl: computed('allocation.id', 'allocation.node.httpAddr', 'useServer', function() {
    const address = this.get('allocation.node.httpAddr');
    const allocation = this.get('allocation.id');

    const url = `/v1/client/fs/logs/${allocation}`;
    return this.useServer ? url : `//${address}${url}`;
  }),

  logParams: computed('task', 'mode', function() {
    return {
      task: this.task,
      type: this.mode,
    };
  }),

  logger: logger('logUrl', 'logParams', function logFetch() {
    // If the log request can't settle in one second, the client
    // must be unavailable and the server should be used instead
    const timing = this.useServer ? this.serverTimeout : this.clientTimeout;
    return url =>
      RSVP.race([this.token.authorizedRequest(url), timeout(timing)]).then(
        response => response,
        error => {
          if (this.useServer) {
            this.set('noConnection', true);
          } else {
            this.send('failoverToServer');
          }
          throw error;
        }
      );
  }),

  actions: {
    setMode(mode) {
      this.logger.stop();
      this.set('mode', mode);
    },
    toggleStream() {
      this.set('streamMode', 'streaming');
      this.toggleProperty('isStreaming');
    },
    gotoHead() {
      this.set('streamMode', 'head');
      this.set('isStreaming', false);
    },
    gotoTail() {
      this.set('streamMode', 'tail');
      this.set('isStreaming', false);
    },
    failoverToServer() {
      this.set('useServer', true);
    },
  },
});
