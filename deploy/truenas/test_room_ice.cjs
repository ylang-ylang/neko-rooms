const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/room-ice.js.template', 'utf8');
test('retained room transport changes without changing credentials or caller config', () => {
  const desired = 'turn:192.168.202.11:22222?transport=tcp';
  const previous = desired.replace('turn:', 'turns:');
  class Native {
    static marker = 'native';
    constructor(config) { this.config = config; }
    setConfiguration(config) { this.config = config; }
  }
  const window = {RTCPeerConnection: Native};
  vm.runInNewContext(source.replace('@TURN_URL_JSON@', JSON.stringify(desired)).replace('@PREVIOUS_TURN_URL_JSON@', JSON.stringify(previous)), {window});
  const config = {iceTransportPolicy: 'all', iceServers: [{urls: [previous, 'stun:other.test:3478'], username: 'existing-user', credential: 'existing-test-secret'}]};
  const pc = new window.RTCPeerConnection(config);
  assert(pc instanceof Native);
  assert.equal(window.RTCPeerConnection.marker, 'native');
  assert.equal(pc.config.iceTransportPolicy, 'all');
  assert.equal(pc.config.iceServers[0].urls[0], desired);
  assert.equal(pc.config.iceServers[0].urls[1], 'stun:other.test:3478');
  assert.equal(pc.config.iceServers[0].credential, config.iceServers[0].credential);
  assert.equal(config.iceServers[0].urls[0], previous);
  pc.setConfiguration({iceServers: [{urls: previous, credential: 'same'}]});
  assert.equal(pc.config.iceServers[0].urls, desired);
  assert.equal(pc.config.iceServers[0].credential, 'same');
});
