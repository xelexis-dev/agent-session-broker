"""Opt-in actual Codex embedded TUI. All auth stays in memory or disposable client state.
Only fixed labels, UUIDs, PID/PPID and payload-presence booleans enter evidence.
"""
import re,fcntl,json,os,pty,select,shlex,shutil,signal,socket,struct,subprocess,sys,tempfile,termios,threading,time,urllib.request
from pathlib import Path
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
SELF=Path(__file__).resolve()
def event(root,label,**fields):
    with (Path(root)/'safe-events.jsonl').open('a') as f:f.write(json.dumps(dict(at=time.time(),label=label,**fields))+'\n')
if len(sys.argv)>1 and sys.argv[1]=='nested':
    root=Path(sys.argv[2]);incoming=json.load(sys.stdin)
    try:fd=os.open(root/'nested-once',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.close(fd)
    except FileExistsError:print('{}');sys.exit()
    child_env=dict(os.environ);child_env['CODEX_THREAD_ID']=incoming['session_id']
    child=subprocess.Popen(['codex','exec','-c','model_provider="gate"','--sandbox','danger-full-access','--skip-git-repo-check','Say nested complete.'],env=child_env,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    event(root,'nested-start',pid=child.pid,ppid=os.getpid(),outerThread=incoming['session_id'])
    try:code=child.wait(timeout=35)
    except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=5);code=-1
    event(root,'nested-exit',pid=child.pid,code=code);print('{}');sys.exit(0 if code==0 else 1)
if len(sys.argv)>1 and sys.argv[1]=='mcp':
    root=Path(sys.argv[2]);s=socket.socket(socket.AF_UNIX);s.connect(str(root/'bridge.sock'));cfg=json.loads(s.makefile().read());s.close()
    phase=cfg['phase'];pid=os.getpid();event(root,'mcp-spawn',phase=phase,pid=pid,ppid=os.getppid(),envThreadPresent=bool(os.getenv('CODEX_THREAD_ID')))
    if cfg['delay']:
        try:fd=os.open(root/'first',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.close(fd);time.sleep(cfg['delay'])
        except FileExistsError:pass
    child=subprocess.Popen([cfg['binary'],'--broker-url',cfg['endpoint'],'--token',cfg['token'],'--state-dir',cfg['state'],'--nickname',cfg['nickname']],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    event(root,'client-start',phase=phase,pid=child.pid,ppid=pid)
    def input_pump():
        try:
            for line in sys.stdin.buffer:
                q=json.loads(line);method=q.get('method')
                if method in ('initialize','notifications/initialized','server/discover'):event(root,method,phase=phase,pid=child.pid)
                if method=='tools/call':event(root,'host-tool',phase=phase,pid=child.pid,thread=q.get('params',{}).get('_meta',{}).get('threadId'),tool=q.get('params',{}).get('name'))
                child.stdin.write(line);child.stdin.flush()
        except (BrokenPipeError,OSError):pass
        finally:
            try:child.stdin.close()
            except OSError:pass
    threading.Thread(target=input_pump,daemon=True).start()
    try:
        for line in child.stdout:
            q=json.loads(line);r=q.get('result',{});sc=r.get('structuredContent',{}) if isinstance(r,dict) else {}
            if isinstance(sc,dict) and ('registered' in sc or 'identitySource' in sc):event(root,'client-status',phase=phase,pid=child.pid,registered=sc.get('registered'),source=sc.get('identitySource'),confirmed=sc.get('identityConfirmed'),receiveError=sc.get('receiveError') if sc.get('receiveError') in ('owned-local','registration-in-use') else None,isError=r.get('isError',False))
            sys.stdout.buffer.write(line);sys.stdout.buffer.flush()
    except (BrokenPipeError,OSError):pass
    finally:
        try:child.wait(timeout=5)
        except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=5)
    event(root,'client-exit',phase=phase,pid=child.pid,code=child.returncode);sys.exit()
config=json.load(sys.stdin);root=Path(tempfile.mkdtemp(prefix='asb-claim-e2e-',dir='/private/tmp'));home=root/'home';ch=home/'.codex';project=root/'project';state=root/'state'
for p in (ch,project,state):p.mkdir(parents=True,exist_ok=True)
subprocess.run(['git','init','-q',str(project)],check=True)
env={k:v for k,v in os.environ.items() if k in ('PATH','LANG','SDKROOT','SHELL')};env.update(HOME=str(home),CODEX_HOME=str(ch),TMPDIR='/private/tmp',TERM='xterm-256color',AGENT_SESSION_BROKER_STATE_DIR=str(state))
settings={'phase':'seed','delay':0,'binary':config['green'],'endpoint':config['endpoint'],'token':config['token'],'state':str(state),'nickname':'claim-u'}
bridge=socket.socket(socket.AF_UNIX);bridge.bind(str(root/'bridge.sock'));os.chmod(root/'bridge.sock',0o600);bridge.listen();closed=False

def serve_bridge():
    while not closed:
        try:
            c,_=bridge.accept()
            with c:c.sendall(json.dumps(dict(settings)).encode())
        except OSError:break
threading.Thread(target=serve_bridge,daemon=True).start()
model_calls={};owned=[];uis=[];observations=[];ui_buffers=[]
class Model(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def do_POST(self):
        data=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))));thread=self.headers.get('thread-id') or data.get('prompt_cache_key');phase=settings['phase'];raw=json.dumps(data.get('input',[]));marker='CLAIM_PAYLOAD_U' in raw
        event(root,'model-input',phase=phase,thread=thread,payloadU=marker,payloadReverse='CLAIM_PAYLOAD_U_REVERSED' in raw,payloadPostV='CLAIM_PAYLOAD_U_POST_V' in raw,payloadYielded='CLAIM_PAYLOAD_U_YIELDED' in raw)
        item={'id':'msg_gate','type':'message','role':'assistant','status':'completed','content':[{'type':'output_text','text':'gate complete','annotations':[]}]}
        self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
        events=[('response.created',{'response':{'id':'resp_gate','object':'response','status':'in_progress','output':[]}}),('response.output_item.added',{'output_index':0,'item':item}),('response.output_text.delta',{'item_id':item['id'],'output_index':0,'content_index':0,'delta':'gate complete'}),('response.output_item.done',{'output_index':0,'item':item}),('response.completed',{'response':{'id':'resp_gate','object':'response','status':'completed','output':[item],'usage':{'input_tokens':1,'output_tokens':1,'total_tokens':2}}})]
        try:
            for name,obj in events:self.wfile.write(('event: '+name+'\ndata: '+json.dumps(dict(type=name,**obj))+'\n\n').encode());self.wfile.flush()
        except BrokenPipeError:pass
model=ThreadingHTTPServer(('127.0.0.1',0),Model);threading.Thread(target=model.serve_forever,daemon=True).start()
def write_config(forward=False):
    text='''model = "gate"
model_provider = "gate"
[model_providers.gate]
name = "Local synthetic gate"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
[mcp_servers.x-broker]
command = "/usr/bin/python3"
args = [%s, "mcp", %s]
%s
[mcp_servers.x-broker.env]
HOME = %s
CODEX_HOME = %s
[projects.%s]
trust_level = "trusted"
'''%(model.server_port,json.dumps(str(SELF)),json.dumps(str(root)),'env_vars = ["CODEX_THREAD_ID"]' if forward else '',json.dumps(str(home)),json.dumps(str(ch)),json.dumps(str(project)))
    (ch/'config.toml').write_text(text)
write_config()
def events():
    p=root/'safe-events.jsonl'
    if not p.exists():return []
    rows=[]
    for line in p.read_text().splitlines():
        try:rows.append(json.loads(line))
        except ValueError:pass
    return rows

def waitfor(label,fn,seconds=12):
    until=time.monotonic()+seconds
    while time.monotonic()<until:
        value=fn()
        if value:return value
        time.sleep(.08)
    raise RuntimeError(label)
def api(path,data=None):
    req=urllib.request.Request(config['origin']+'/probe/'+path,data=json.dumps(data).encode() if data is not None else None,headers={'Content-Type':'application/json'})
    with urllib.request.urlopen(req,timeout=12) as r:return json.load(r)
def row(name):return next((r for r in api('roster') if r['name']==name),None)
def ready(name,after=-1):r=row(name);return r if r and r.get('automaticDeliveryReady') and r.get('status')=='alive' and (r.get('generation') or 0)>after else None
def generation(name):r=row(name);return (r.get('generation') or 0) if r else 0
def write_hooks(seed=False,nested=False):
    ingress={'type':'mcp_tool','server':'x-broker','tool':'ingress_hook','input':{'session_id':'${session_id}','hook_event_name':'${hook_event_name}','turn_id':'${turn_id}'},'timeout':10}
    hooks={'UserPromptSubmit':[{'hooks':[ingress,{'type':'mcp_tool','server':'x-broker','tool':'auth_status','input':{},'timeout':10}]}]}
    if nested:hooks['UserPromptSubmit'][0]['hooks'].append({'type':'command','command':shlex.join(['/usr/bin/python3',str(SELF),'nested',str(root)]),'timeout':45})
    if seed:hooks['SessionStart']=[{'hooks':[{'type':'mcp_tool','server':'x-broker','tool':'attach_session','input':{},'timeout':10}]}]
    (ch/'hooks.json').write_text(json.dumps({'hooks':hooks}))

def start(phase,u=None,binary=None,delay=0,outer=None):
    settings.update(phase=phase,binary=binary or config['green'],delay=delay);(root/'first').unlink(missing_ok=True);write_hooks(seed=phase.startswith('seed'),nested=phase=='nested-exec')
    a,b=pty.openpty();fcntl.ioctl(b,termios.TIOCSWINSZ,struct.pack('HHHH',36,140,0,0));args=['codex','-c','model_provider="gate"','--no-alt-screen','--sandbox','danger-full-access','--ask-for-approval','never']
    if u:args=['sh',config['launcher'],u]+args[1:]
    child_env=dict(env)
    if outer:child_env['CODEX_THREAD_ID']=outer
    p=subprocess.Popen(args,stdin=b,stdout=b,stderr=b,cwd=project,env=child_env,start_new_session=True);os.close(b);owned.append(p);stop=threading.Event();uis.append((p,a,stop))
    ui_buffer=bytearray();ui_buffers.append(ui_buffer)
    def drain():
        hook_trust_accepted=False
        while not stop.is_set():
            try:
                if select.select([a],[],[],.1)[0]:
                    data=os.read(a,65536)
                    if not data:break
                    ui_buffer.extend(data)
                    if len(ui_buffer)>16000:del ui_buffer[:-16000]
                    if b'\x1b[6n' in data:os.write(a,b'\x1b[1;1R')
                    # Only fixed diagnostic booleans; never retain raw terminal text.
                    clean=re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',data.decode(errors='ignore')).lower()
                    if ('hooks need review' in clean or 'hooks are new or changed' in clean) and not hook_trust_accepted:
                        hook_trust_accepted=True
                        event(root,'isolated-hook-trust-all',phase=phase,pid=p.pid)
                        time.sleep(.6)
                        os.write(a,b'2\r')

            except OSError:break
    threading.Thread(target=drain,daemon=True).start();event(root,'tui-start',phase=phase,pid=p.pid);return p,a

def send(fd,text):os.write(fd,text.encode());time.sleep(.65);os.write(fd,b'\r')
def close(p):
    try:os.killpg(p.pid,signal.SIGTERM)
    except ProcessLookupError:pass
    try:p.wait(timeout=5)
    except subprocess.TimeoutExpired:os.killpg(p.pid,signal.SIGKILL);p.wait(timeout=5)
    time.sleep(.3)
def initialized(phase):return any(e['label'] in ('notifications/initialized','server/discover') and e.get('phase')==phase for e in events())
def host_tool(phase,name,thread=None):return next((e for e in events() if e['label']=='host-tool' and e['phase']==phase and e['tool']==name and (thread is None or e.get('thread')==thread)),None)
def marker_seen(phase,thread):
    key='payloadReverse' if phase=='reversed' else 'payloadPostV' if phase=='post-V-delivery' else 'payloadU'
    return any(e['label']=='model-input' and e['phase']==phase and e.get('thread')==thread and e.get(key) for e in events())
def snapshot(thread):
    holder=api('holder?thread='+thread);current=row('claim-u') or {}
    return {'holderPID':holder['holderPID'],'generation':holder['generation'],'ready':bool(current.get('automaticDeliveryReady')),'status':current.get('status')}
def release_records(thread,pid,after):
    return [e for e in api('events') if e['tool']=='report_ingress_failure' and e['thread']==thread and e['holderPID']==pid and e['at']>=after]
def native(u,message):
    p=subprocess.run(['codex','queue','--thread',u,'--message',message],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=12)
    assert p.returncode==0,'native queue'
def note(label,**kw):observations.append(dict(label=label,**kw));print(json.dumps(observations[-1]),flush=True)
success=False
try:
    p,fd=start('seed-u');waitfor('fresh-initialize',lambda:initialized('seed-u'));time.sleep(2)
    assert row('claim-u') is None,'fresh auto-registered'
    send(fd,'Connect this conversation to x-broker by calling attach_session once.')
    call=waitfor('fresh-real-attach',lambda:host_tool('seed-u','attach_session'))
    u=call['thread'];original=waitfor('fresh-ready',lambda:ready('claim-u'));rid=original['registrationId'];close(p)
    note('fresh-explicit-attach',noAutoRegister=True)
    p,fd=start('baseline-red',u,config['red']);waitfor('red-initialize',lambda:initialized('baseline-red'));time.sleep(5)
    assert not ready('claim-u',original['generation']),'baseline unexpectedly resumed';assert not any(e['thread']==u and e['tool']=='resume_session' for e in api('events'));assert not host_tool('baseline-red','auth_status')
    note('develop-RED',reason='MCP initialized but saved registration unavailable before first tool');close(p)
    before=generation('claim-u');started=time.monotonic();p,fd=start('normal-green',u);r=waitfor('green-ready',lambda:ready('claim-u',before),5)
    assert r['registrationId']==rid;assert not any(e['label']=='model-input' and e['phase']=='normal-green' for e in events())
    note('promptless-GREEN',readyMs=round((time.monotonic()-started)*1000))
    assert api('send',{'To':'claim-u','Body':'CLAIM_PAYLOAD_U'})['accepted'];waitfor('green-model-delivery',lambda:marker_seen('normal-green',u),16)
    assert any(e['label']=='client-status' and e.get('phase')=='normal-green' and e.get('source')=='launcher-claim' and e.get('confirmed') is True for e in events()),'confirmed auth_status missing'
    note('normal-native-delivery',toU=True,claimConfirmedByAuthStatus=True)
    # /new after normal claim consumption cannot consume that claim again.
    revision=row('claim-u')['revision'];send(fd,'/new');time.sleep(.8);os.write(fd,b'\r');time.sleep(2)
    assert ready('claim-u') and row('claim-u')['revision']==revision
    note('normal-new',keepsU=True);close(p)
    # A second valid saved thread makes inherited B a meaningful negative case.
    settings['nickname']='claim-b';p,fd=start('seed-b');waitfor('B-initialize',lambda:initialized('seed-b'));time.sleep(2);send(fd,'Connect this conversation by calling attach_session once.')
    bcall=waitfor('B-attach',lambda:host_tool('seed-b','attach_session'));b=bcall['thread'];brow=waitfor('B-ready',lambda:ready('claim-b'));close(p);settings['nickname']='claim-u'
    bgen=row('claim-b')['generation'];write_config(True)
    before=generation('claim-u');p,fd=start('nested-env',u,outer=b);waitfor('nested-env-U-ready',lambda:ready('claim-u',before),5)
    assert row('claim-u')['registrationId']==rid and row('claim-b')['generation']==bgen
    assert all(not e['envThreadPresent'] for e in events() if e['label']=='mcp-spawn' and e['phase']=='nested-env')
    note('nested-env',onlyUResumed=True);close(p);write_config()
    before=generation('claim-u');p,fd=start('nested-exec',u);waitfor('nested-parent-ready',lambda:ready('claim-u',before),5);parent_generation=generation('claim-u')
    send(fd,'Run the isolated nested Codex hook and say gate complete.')
    nested=waitfor('nested-Codex-exit',lambda:next((e for e in events() if e['label']=='nested-exit'),None),40)
    assert nested['code']==0 and generation('claim-u')==parent_generation,'nested Codex stole U'
    assert len([e for e in events() if e['label']=='mcp-spawn' and e['phase']=='nested-exec'])>=2,'nested MCP did not start'
    note('nested-exec',closestParentIsNested=True,UUnchanged=True);close(p)
    before=generation('claim-u');p,fd=start('reversed',u,delay=5);time.sleep(.8);send(fd,'/new');time.sleep(.8);os.write(fd,b'\r')
    waitfor('reverse-U-ready',lambda:ready('claim-u',before),8);time.sleep(5)
    mcp=[e for e in events() if e['label']=='client-start' and e['phase']=='reversed'];assert len(mcp)==2,'reverse two MCP clients';claim_snapshot=snapshot(u);assert claim_snapshot['holderPID']==mcp[0]['pid'],'wrong native claim consumer';assert not any(e['label']=='host-tool' and e['phase']=='reversed' and e['pid']==mcp[0]['pid'] for e in events()),'claim consumer had metadata before holder snapshot'
    # Drive V metadata before U's hook can request yield; test mismatch release independently.
    before_v=snapshot(u);settings['phase']='reverse-release';send(fd,'Say gate complete.')
    # Existing MCP wrappers retain their spawn phase; host metadata is authoritative.
    vcall=waitfor('V-first-meta',lambda:next((e for e in events() if e['label']=='host-tool' and e.get('phase')=='reversed' and e['tool']=='ingress_hook' and e.get('thread') not in (u,b)),None));v=vcall['thread']
    assert vcall['pid']==mcp[0]['pid'],'claim consumer was not the earlier actual client (later wrapper)'
    # Metadata need not create an observable not-ready interval; observe the holder.
    deadline=vcall['at']+8
    release=waitfor('V-release-outcome',lambda:release_records(u,mcp[0]['pid'],vcall['at']),max(.1,deadline-time.time()))
    after_v=snapshot(u)
    assert after_v['holderPID']!=mcp[0]['pid'],'V-retains-U-holder'
    event(root,'holder-snapshot',beforeV=before_v,afterV=after_v,claimGeneration=claim_snapshot['generation'],release=release)
    settings['phase']='restore-u';restore_at=time.time();restore_deadline=restore_at+16;native(u,'CLAIM_CALL_U: gate text.')
    waitfor('U-real-meta',lambda:next((e for e in events() if e['label']=='host-tool' and e['phase']=='reversed' and e['tool']=='ingress_hook' and e.get('thread')==u and e['at']>restore_at),None),max(.1,restore_deadline-time.time()))
    def own_holder():
        current=snapshot(u)
        return current if current['holderPID']==mcp[1]['pid'] and current['generation']>claim_snapshot['generation'] and current['ready'] else None
    try:after_u=waitfor('U-post-V-holder',own_holder,max(.1,restore_deadline-time.time()))
    except RuntimeError:
        current=snapshot(u);event(root,'holder-failure-snapshot',beforeV=before_v,afterV=current,release=release)
        raise RuntimeError('V-retains-U-holder' if current['holderPID']==mcp[0]['pid'] else 'U-post-V-restore')
    waitfor('V-real-model-input',lambda:any(e['label']=='model-input' and e.get('phase') in ('reverse-release','restore-u') and e.get('thread')==v for e in events()),12)
    assert not any(e['label']=='model-input' and e.get('thread')==v and e['payloadU'] for e in events()),'U message appeared in V'
    settings['phase']='post-V-delivery';assert api('send',{'To':'claim-u','Body':'CLAIM_PAYLOAD_U_POST_V'})['accepted'];waitfor('post-V-peer-delivery',lambda:marker_seen('post-V-delivery',u),16)
    event(root,'holder-final-snapshot',afterU=after_u,ownPID=mcp[1]['pid'],formerClaimPID=mcp[0]['pid'])
    note('reversed-order',wrongConsumer=True,deliveredOnlyU=True,VFirstMetaReleased=True,UFirstMetaResumed=True,restoreMs=round((time.time()-restore_at)*1000))
    close(p)
    before=generation('claim-u');p,fd=start('yield-reversed',u,delay=5);time.sleep(.8);send(fd,'/new');time.sleep(.8);os.write(fd,b'\r')
    waitfor('reverse-U-ready',lambda:ready('claim-u',before),8);time.sleep(5)
    mcp=[e for e in events() if e['label']=='client-start' and e['phase']=='yield-reversed'];assert len(mcp)==2,'reverse two MCP clients';claim_snapshot=snapshot(u);assert claim_snapshot['holderPID']==mcp[0]['pid'],'wrong native claim consumer';assert not any(e['label']=='host-tool' and e['phase']=='yield-reversed' and e['pid']==mcp[0]['pid'] for e in events()),'claim consumer had metadata before holder snapshot'
    assert not any(e['label']=='model-input' and e.get('phase')=='yield-reversed' and e.get('thread')!=u for e in events()),'V already had model input'
    settings['phase']='claim-yield';native(u,'YIELD_CALL_U: say gate text.')
    ucall=waitfor('U-authoritative-meta',lambda:next((e for e in events() if e['label']=='host-tool' and e.get('phase')=='yield-reversed' and e['pid']==mcp[1]['pid'] and e['tool']=='ingress_hook' and e.get('thread')==u),None),16)
    def yielded_holder():
        current=snapshot(u)
        return current if current['holderPID']==mcp[1]['pid'] and current['generation']>claim_snapshot['generation'] and current['ready'] else None
    deadline=ucall['at']+8
    try:after_u=waitfor('U-claim-yield-holder',yielded_holder,max(.1,deadline-time.time()))
    except RuntimeError:
        blocked=[e.get('receiveError') for e in events() if e['label']=='client-status' and e.get('receiveError')]
        event(root,'yield-RED-snapshot',before=claim_snapshot,after=snapshot(u),firstBlock=blocked[0] if blocked else 'unclassified',controlEvents=api('events')[-12:],UClient=mcp[1]['pid'],VClient=mcp[0]['pid'])
        raise
    handoff_ms=round((time.time()-ucall['at'])*1000)
    assert len(release_records(u,mcp[0]['pid'],ucall['at']))==1,'yield release reported more than once'
    peer_send_at=time.time();assert api('send',{'To':'claim-u','Body':'CLAIM_PAYLOAD_U_YIELDED'})['accepted']
    yielded_input=waitfor('yield-peer-model-input',lambda:next((e for e in events() if e['label']=='model-input' and e.get('phase')=='claim-yield' and e.get('thread')==u and e.get('payloadYielded') and e['at']>peer_send_at),None),16)
    vmodels=[e for e in events() if e['label']=='model-input' and e.get('phase') in ('yield-reversed','claim-yield') and e.get('thread')!=u]
    assert len(vmodels)==0,'V received model input'
    event(root,'yield-GREEN-snapshot',before=claim_snapshot,after=after_u,UClient=mcp[1]['pid'],VClient=mcp[0]['pid'],VModelRequests=0,afterUMetaMs=handoff_ms,peerSendAt=peer_send_at,peerModelInputAt=yielded_input['at'],peerReceiptMs=round((yielded_input['at']-peer_send_at)*1000))
    note('claim-yield',sameRegistration=row('claim-u')['registrationId']==rid,deliveredOnlyU=True,VModelRequests=0);close(p)
    # Launcher-free resume keeps the documented first-tool fallback.
    p,fd=start('fallback',None);close(p) # fresh control already asserted above
    # Exact resume without launcher (same process mode/config).
    settings['phase']='fallback-prompt'
    # Use a local shim that merely execs Codex resume, leaving no claim.
    fallback=root/'plain-resume';fallback.write_text('#!/bin/sh\nexec codex resume "$@"\n');fallback.chmod(0o700)
    old=config['launcher'];config['launcher']=str(fallback)
    before=generation('claim-u');p,fd=start('fallback',u);config['launcher']=old;waitfor('fallback-initialize',lambda:initialized('fallback'));time.sleep(5);assert not ready('claim-u',before)
    settings['phase']='fallback-prompt';send(fd,'Call auth_status once.');waitfor('fallback-ready',lambda:ready('claim-u',before),12)
    note('launcher-free-resume',waitsForFirstTool=True);close(p)
    success=True
except Exception as e:
    # Fixed assertion labels only; never format raw subprocess, HTTP or config errors.
    label=str(e) if isinstance(e,(AssertionError,RuntimeError)) else type(e).__name__
    note('failure',reason=label[:100])
    if 'before_v' in locals():event(root,'holder-stop-snapshot',beforeV=before_v,afterV=snapshot(u),release=release_records(u,mcp[0]['pid'],vcall['at']) if 'vcall' in locals() else [])
    for buf in ui_buffers[-1:]:
        text=re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',bytes(buf).decode(errors='ignore'))
        text=re.sub(r'asb_[psu]_[A-Za-z0-9_-]+','<redacted>',text)
        text=re.sub(r'/[^\s\x1b]+','<path>',text)
        snippets=[m.group(0) for m in re.finditer(r'.{0,25}(?:hook|trust|error|press|continue).{0,90}',text,re.I)]
        note('isolated-hook-ui-diagnostic',snippets=snippets[-12:])

finally:
    for p in owned:
        if p.poll() is None:close(p)
    for _,fd,stop in uis:
        stop.set()
        try:os.close(fd)
        except OSError:pass
    # Child transports may own process groups; terminate only PIDs recorded by this fixture.
    for e in events():
        if e['label'] in ('mcp-spawn','client-start'):
            try:os.kill(e['pid'],signal.SIGTERM)
            except ProcessLookupError:pass
    closed=True;bridge.close();model.shutdown();model.server_close()
    if config.get('evidence'):
        dest=Path(config['evidence']);dest.mkdir(parents=True,exist_ok=True)
        (dest/'safe-events.jsonl').write_text((root/'safe-events.jsonl').read_text())
        (dest/'result.json').write_text(json.dumps({'success':success,'observations':observations},indent=2))
    shutil.rmtree(root)
sys.exit(0 if success else 1)
