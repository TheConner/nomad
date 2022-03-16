import { inject as service } from '@ember/service';
import { get } from '@ember/object';
import ApplicationSerializer from './application';
import classic from 'ember-classic-decorator';

@classic
export default class EvaluationStub extends ApplicationSerializer {
  @service system;

  mapToArray = ['FailedTGAllocs'];
  separateNanos = ['CreateTime', 'ModifyTime'];

  normalize(typeHash, hash) {
    console.log(hash);
    console.log(typeHash);
    hash.EvalId = hash.ID;
    hash.PlainJobId = hash.JobID;
    hash.Namespace = hash.Namespace || get(hash, 'Job.Namespace') || 'default';
    hash.JobID = JSON.stringify([hash.JobID, hash.Namespace]);

    return super.normalize(typeHash, hash);
  }
}
