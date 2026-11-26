const test=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),fs=require('node:fs');
const code=fs.readFileSync(__dirname+'/tls-warning.js','utf8');
const turn='turns:neko.example:8443?transport=tcp';
const cfg={iceServers:[{urls:[turn],username:'kept',credential:'kept'}],iceTransportPolicy:'all'};
function fixture(protocol='https:',dataset={}) {
  class Node {
    constructor(tag){this.tagName=tag;this.children=[];this.attributes={};this.style={};this.events={};}
    setAttribute(k,v){this.attributes[k]=v;}
    addEventListener(k,v){this.events[k]=v;}
    append(...nodes){for(const n of nodes){n.parent=this;this.children.push(n);}}
    remove(){this.parent.children=this.parent.children.filter(x=>x!==this);}
  }
  class Peer {
    static generateCertificate(){return 'native-static';}
    constructor(config){this.config=config;this.events={};this.connectionState='new';this.iceConnectionState='new';}
    setConfiguration(config){this.config=config;return 'native-result';}
    getConfiguration(){return this.config;}
    addEventListener(k,v){(this.events[k]||=[]).push(v);}
    emit(k,v={}){for(const f of this.events[k]||[])f(v);}
  }
  const body=new Node('body'),events={},timers=new Map();let id=0;
  const document={body,documentElement:body,currentScript:{dataset},createElement:tag=>new Node(tag),addEventListener:(k,v)=>events[k]=v};
  const location={protocol,host:'neko.example:8443',hostname:'neko.example',origin:protocol+'//neko.example:8443',href:protocol+'//neko.example:8443/room/test/'};
  const window={RTCPeerConnection:Peer,location};
  const sandbox=vm.createContext({window,document,location,navigator:{userAgent:'Chrome'},URL,setTimeout:(f,ms)=>{timers.set(++id,{f,ms});return id;},clearTimeout:id=>timers.delete(id)});vm.runInContext(code,sandbox);
  const flatten=n=>[n,...n.children.flatMap(flatten)];
  return {Peer,window,body,events,timers,sandbox,flush(){for(const[id,{f}]of[...timers]){timers.delete(id);f();}},nodes(){return flatten(body);},text(){return flatten(body).map(x=>x.textContent||'').join(' ');},banner(){return body.children.find(x=>'data-neko-tls-warning'in x.attributes);},pill(){return body.children.find(x=>'data-neko-tls-warning-pill'in x.attributes);}};
}
const fail=p=>p.emit('icecandidateerror',{url:turn,errorCode:701,errorText:'Failed to establish connection'});
test('HTTP never wraps or installs warning listeners',()=>{const f=fixture('http:');assert.equal(f.window.RTCPeerConnection,f.Peer);assert.deepEqual(Object.keys(f.events),[]);});
test('no error or non-TLS 701 does not warn',()=>{const f=fixture(),p=new f.window.RTCPeerConnection(cfg);f.flush();assert.equal(f.banner(),undefined);p.emit('icecandidateerror',{url:'turn:neko.example:8443',errorCode:701});f.flush();assert.equal(f.banner(),undefined);});
test('explicit TURNS failure without relay waits, then offers conditional CA help',()=>{const f=fixture(),p=new f.window.RTCPeerConnection(cfg);fail(p);assert.equal(f.banner(),undefined);assert.equal([...f.timers.values()][0].ms,3000);f.flush();assert.ok(f.banner());assert.match(f.text(),/Chromium:/);assert.match(f.text(),/Firefox:/);assert.match(f.text(),/DNS, network or server/);assert.ok(!f.nodes().some(n=>n.href?.startsWith('http:')));assert.ok(f.nodes().some(n=>n.href==='https://neko.example:8443/ca.crt'));});
test('relay candidate or connected peer suppresses transient errors',()=>{for(const kind of['relay','connected']){const f=fixture(),p=new f.window.RTCPeerConnection(cfg);fail(p);if(kind==='relay')p.emit('icecandidate',{candidate:{type:'relay'}});else{p.connectionState='connected';p.emit('connectionstatechange');}f.flush();assert.equal(f.banner(),undefined);}});
test('failed connection retry remains visible until a working replacement',()=>{const f=fixture(),p=new f.window.RTCPeerConnection(cfg);fail(p);p.connectionState='closed';p.emit('connectionstatechange');f.flush();assert.ok(f.banner());const q=new f.window.RTCPeerConnection(cfg);q.emit('icecandidate',{candidate:{candidate:'candidate:x 1 udp 1 127.0.0.1 1 typ relay'}});assert.equal(f.banner(),undefined);});
test('healthy alternate connection prevents a warning',()=>{const f=fixture(),p=new f.window.RTCPeerConnection(cfg),q=new f.window.RTCPeerConnection(cfg);q.connectionState='connected';q.emit('connectionstatechange');fail(p);f.flush();assert.equal(f.banner(),undefined);});
test('only explicitly configured HTTP alternative is linked',()=>{const f=fixture('https:',{httpUrl:'http://neko.example:22222/manage/'}),p=new f.window.RTCPeerConnection(cfg);fail(p);f.flush();assert.ok(f.nodes().some(n=>n.href==='http://neko.example:22222/manage/'));});
test('native config, return value and static API remain untouched',()=>{const f=fixture(),p=new f.window.RTCPeerConnection(cfg);assert.equal(p.config,cfg);assert.equal(f.window.RTCPeerConnection.generateCertificate(),'native-static');assert.equal(p.setConfiguration(cfg),'native-result');assert.equal(p.config,cfg);});
test('removing TURNS cancels obsolete failure help',()=>{const f=fixture(),p=new f.window.RTCPeerConnection(cfg);fail(p);p.setConfiguration({iceServers:[]});f.flush();assert.equal(f.banner(),undefined);});
test('dismissal collapses to a persistent yellow pill, re-opens on click, and clears once connected',()=>{const f=fixture(),p=new f.window.RTCPeerConnection(cfg);fail(p);f.flush();let stopped=false;f.events.keydown({target:{closest:()=>f.banner()},stopImmediatePropagation:()=>stopped=true});assert.equal(stopped,true);f.nodes().find(n=>n.attributes['aria-label']==='Dismiss TURN/TLS warning').events.click();assert.equal(f.banner(),undefined);assert.ok(f.pill());fail(p);f.flush();assert.equal(f.banner(),undefined);assert.ok(f.pill());f.pill().events.click();assert.ok(f.banner());assert.equal(f.pill(),undefined);f.nodes().find(n=>n.attributes['aria-label']==='Dismiss TURN/TLS warning').events.click();assert.ok(f.pill());p.emit('icecandidate',{candidate:{type:'relay'}});assert.equal(f.banner(),undefined);assert.equal(f.pill(),undefined);});

test('manage surface shows immediate CA notice, universal Linux/macOS/Windows CLI commands, and collapses to yellow pill',()=>{const f=fixture('https:',{surface:'manage',caUrl:'/ca.crt'});assert.ok(f.banner());assert.match(f.text(),/Private CA required for HTTPS room video/);assert.match(f.text(),/http:\/\/neko\.example:8443\/ca\.crt/);assert.match(f.text(),/trust anchor --store/);assert.match(f.text(),/install -D -m 644/);assert.match(f.text(),/\.var\/app\/\*\/config\/pki\/nssdb/);assert.ok(f.nodes().some(n=>n.attributes['data-neko-copy-ca-cmd']!==undefined));assert.ok(f.nodes().some(n=>n.href==='https://neko.example:8443/ca.crt'));f.nodes().find(n=>n.attributes['aria-label']==='Dismiss TURN/TLS warning').events.click();assert.equal(f.banner(),undefined);assert.ok(f.pill());f.pill().events.click();assert.ok(f.banner());const noCa=fixture('https:',{surface:'manage',caUrl:''});assert.equal(noCa.banner(),undefined);assert.equal(noCa.pill(),undefined);const plain=fixture('http:',{surface:'manage',caUrl:'/ca.crt'});assert.equal(plain.banner(),undefined);assert.equal(plain.pill(),undefined);});
test('TLS watcher sees the preferred URL after retained-room transport rewrite',()=>{const f=fixture();const previous=turn.replace('turns:','turn:');const shim=fs.readFileSync(__dirname+'/room-ice.js.template','utf8').replace('@TURN_URL_JSON@',JSON.stringify(turn)).replace('@PREVIOUS_TURN_URL_JSON@',JSON.stringify(previous));vm.runInContext(shim,f.sandbox);const original={iceServers:[{urls:[previous],username:'kept',credential:'kept'}]};const p=new f.window.RTCPeerConnection(original);assert.equal(p.getConfiguration().iceServers[0].urls[0],turn);assert.equal(original.iceServers[0].urls[0],previous);fail(p);f.flush();assert.ok(f.banner());const nginx=fs.readFileSync(__dirname+'/nginx.conf.template','utf8');assert.ok(nginx.indexOf('src="/.neko/tls-warning.js"')<nginx.indexOf('src="/.neko/room-ice.js"'));});
