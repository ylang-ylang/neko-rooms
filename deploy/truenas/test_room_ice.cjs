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

test('firefox numeric-IP turns falls back to plain turn on same port while chrome and firefox FQDN keep turns', () => {
  class Native {
    constructor(config) { this.config = config; }
    setConfiguration(config) { this.config = config; }
  }
  const ipTurns = 'turns:192.168.202.11:22222?transport=tcp';
  const ipTurn = 'turn:192.168.202.11:22222?transport=tcp';
  const render = (desired, previous) => source.replace('@TURN_URL_JSON@', JSON.stringify(desired)).replace('@PREVIOUS_TURN_URL_JSON@', JSON.stringify(previous));

  // Firefox + numeric IP turns -> rewrites BOTH turns and retained turn to plain turn:
  const ffWindow = {RTCPeerConnection: Native};
  vm.runInNewContext(render(ipTurns, ipTurn), {window: ffWindow, navigator: {userAgent: 'Mozilla/5.0 Gecko/20100101 Firefox/153.0'}});
  const ffPc = new ffWindow.RTCPeerConnection({iceServers: [{urls: [ipTurns], username: 'u', credential: 'p'}]});
  assert.equal(ffPc.config.iceServers[0].urls[0], ipTurn);
  ffPc.setConfiguration({iceServers: [{urls: ipTurn, username: 'u', credential: 'p'}]});
  assert.equal(ffPc.config.iceServers[0].urls, ipTurn);

  // Chrome + numeric IP turns -> keeps turns: (and upgrades retained turn: -> turns:)
  const chWindow = {RTCPeerConnection: Native};
  vm.runInNewContext(render(ipTurns, ipTurn), {window: chWindow, navigator: {userAgent: 'Mozilla/5.0 Chrome/156.0.0.0 Safari/537.36'}});
  const chPc = new chWindow.RTCPeerConnection({iceServers: [{urls: [ipTurns, ipTurn], username: 'u', credential: 'p'}]});
  assert.deepEqual(chPc.config.iceServers[0].urls, [ipTurns, ipTurns]);

  // Firefox + FQDN turns -> keeps turns:
  const fqdnTurns = 'turns:neko.private.test:22222?transport=tcp';
  const fqdnTurn = 'turn:neko.private.test:22222?transport=tcp';
  const ffFqdnWindow = {RTCPeerConnection: Native};
  vm.runInNewContext(render(fqdnTurns, fqdnTurn), {window: ffFqdnWindow, navigator: {userAgent: 'Mozilla/5.0 Gecko/20100101 Firefox/153.0'}});
  const ffFqdnPc = new ffFqdnWindow.RTCPeerConnection({iceServers: [{urls: [fqdnTurns], username: 'u', credential: 'p'}]});
  assert.equal(ffFqdnPc.config.iceServers[0].urls[0], fqdnTurns);
});
