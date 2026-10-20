"use strict";
const assert = require("node:assert/strict");
const {indexClusterPresentation} = require("./static/app.js");
  for (const count of [0, 2]) {
    const legacy = indexClusterPresentation({runningClusters: count});
    const measured = indexClusterPresentation({runningClusters: count, runningClustersKnown: true});
    assert.deepEqual(legacy, measured);
    assert.equal(measured.hidden, count === 0);
    assert.equal(measured.text, `${count} running clusters`);
  }
  const unknown = {runningClusters:0, runningClustersKnown:false};
  // Baseline assets hide the integer fallback; new assets clear any stale text.
  assert.equal(unknown.runningClusters === 0, true);
  assert.deepEqual(indexClusterPresentation(unknown), {text:"", title:"", hidden:true});
  assert.equal(indexClusterPresentation({...unknown, clusterNotice:"Genuine failure"}).hidden, false);
