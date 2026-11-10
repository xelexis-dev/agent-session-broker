"""Real isolated daemon resume; fixed labels/PIDs/UUIDs only, no raw state/argv."""
import base64,fcntl,hashlib,json,os,pty,re,select,shutil,signal,socket,struct,subprocess,sys,tempfile,termios,threading,time,urllib.request
from pathlib import Path
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
SELF=Path(__file__).resolve()
def emit(label,**fields):print(json.dumps(dict(label=label,**fields)),flush=True)
def event(root,label,**fields):
    with (root/'events').open('a') as stream:stream.write(json.dumps(dict(label=label,**fields))+'\n')
class WS:
    def __init__(self,path):
        self.s=socket.socket(socket.AF_UNIX);self.s.settimeout(15);self.s.connect(str(path));self.buf=b'';self.seq=0;self.notifications=[]
        key=base64.b64encode(os.urandom(16)).decode();self.s.sendall(('GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: '+key+'\r\nSec-WebSocket-Version: 13\r\n\r\n').encode())
        while b'\r\n\r\n' not in self.buf:self.buf+=self.s.recv(65536)
        head,self.buf=self.buf.split(b'\r\n\r\n',1);assert b' 101 ' in head.split(b'\r\n')[0]
        r=self.call('initialize',{'clientInfo':{'name':'daemon-probe','version':'1'},'capabilities':{'experimentalApi':True}});assert 'error' not in r
        self.send({'jsonrpc':'2.0','method':'initialized'})
    def frame(self,op,data):
        mask=os.urandom(4);n=len(data);head=bytes([0x80|op])
        head+=bytes([0x80|n]) if n<126 else bytes([0x80|126])+struct.pack('>H',n) if n<65536 else bytes([0x80|127])+struct.pack('>Q',n)
        self.s.sendall(head+mask+bytes(v^mask[i%4] for i,v in enumerate(data)))
    def send(self,obj):self.frame(1,json.dumps(obj).encode())
    def need(self,n,end):
        while len(self.buf)<n:
            self.s.settimeout(max(.01,end-time.monotonic()));b=self.s.recv(65536)
            if not b:raise RuntimeError('ws-closed')
            self.buf+=b
    def read(self,end):
        result=b''
        while True:
            self.need(2,end);a,b=self.buf[:2];n=b&127;off=2
            if n==126:self.need(4,end);n=struct.unpack('>H',self.buf[2:4])[0];off=4
            elif n==127:self.need(10,end);n=struct.unpack('>Q',self.buf[2:10])[0];off=10
            self.need(off+n,end);data=self.buf[off:off+n];self.buf=self.buf[off+n:];op=a&15
            if op==9:self.frame(10,data);continue
            if op==8:raise RuntimeError('ws-close-frame')
            if op in (0,1,2):
                result+=data
                if a&128:return json.loads(result)
    def call(self,method,params,timeout=20):
        self.seq+=1;seq=self.seq;self.send({'jsonrpc':'2.0','id':seq,'method':method,'params':params});end=time.monotonic()+timeout
        while True:
            d=self.read(end)
            if d.get('id')==seq and ('result' in d or 'error' in d):
                return d
            if 'method' in d:
                self.notifications.append(d)
                if 'id' in d:self.send({'jsonrpc':'2.0','id':d['id'],'error':{'code':-32601,'message':'probe declines request'}})
    def close(self):
        try:self.frame(8,b'')
        except OSError:pass
        self.s.close()



if len(sys.argv)>1 and sys.argv[1]=='mcp':
    root=Path(sys.argv[2]);sock=socket.socket(socket.AF_UNIX);sock.connect(str(root/'bridge.sock'));cfg=json.loads(sock.makefile().read());sock.close()
    child=subprocess.Popen([cfg['binary'],'--broker-url',cfg['endpoint'],'--token',cfg['token'],'--state-dir',str(root/'state'),'--nickname',cfg['name']],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    event(root,'native-start',phase=cfg['phase'],pid=child.pid,ppid=os.getpid(),hostParentPID=os.getppid())
    pending={}
    def pump():
        try:
            for line in sys.stdin.buffer:
                q=json.loads(line)
                if q.get('method')=='notifications/initialized':event(root,'initialized',phase=cfg['phase'],pid=child.pid)
                if q.get('method')=='tools/call':
                    p=q.get('params',{});pending[q['id']]=p.get('name')
                    event(root,'host-tool',phase=cfg['phase'],pid=child.pid,tool=p.get('name'),thread=p.get('_meta',{}).get('threadId'))
                child.stdin.write(line);child.stdin.flush()
        except (BrokenPipeError,OSError):pass
        finally:
            try:child.stdin.close()
            except OSError:pass
    threading.Thread(target=pump,daemon=True).start()
    try:
        for line in child.stdout:
            q=json.loads(line);r=q.get('result',{});data=r.get('structuredContent',{}) if isinstance(r,dict) else {};name=pending.pop(q.get('id'),None)
            if name=='auth_status' and isinstance(data,dict):event(root,'status',phase=cfg['phase'],pid=child.pid,registered=data.get('registered'),receiverReady=data.get('receiverReady'),status=data.get('status'),deviceIdentity=data.get('deviceIdentity'))
            sys.stdout.buffer.write(line);sys.stdout.buffer.flush()
    except (BrokenPipeError,OSError):pass
    try:child.wait(timeout=6)
    except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=5)
    event(root,'native-exit',phase=cfg['phase'],pid=child.pid);sys.exit()
cfg=json.load(sys.stdin);base=Path(tempfile.mkdtemp(prefix='dr-e2e-',dir='/private/tmp'));uis=[];current={};bridge=None;model=None;calls=[];controllers=[];model_events=[]
def wait(label,predicate,seconds=20):
    until=time.monotonic()+seconds
    while time.monotonic()<until:
        value=predicate()
        if value:return value
        time.sleep(.05)
    raise RuntimeError(label)
def api(path):
    with urllib.request.urlopen(cfg['origin']+'/probe/'+path,timeout=12) as response:return json.load(response)
def rows(root,label=None,phase=None):
    if not (root/'events').exists():return []
    out=[]
    for line in (root/'events').read_text().splitlines():
        try:row=json.loads(line)
        except ValueError:continue
        if (label is None or row['label']==label) and (phase is None or row.get('phase')==phase):out.append(row)
    return out
def roster(thread):return next((r for r in api('roster') if r['id']==thread),None)
def holder(thread):return api('holder?thread='+thread)
class Model(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def do_POST(self):
        data=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))));thread=self.headers.get('thread-id') or data.get('prompt_cache_key');calls.append(thread);request_text=json.dumps(data).lower();model_events.append({'phase':current.get('phase'),'thread':thread,'at':time.monotonic(),'hasThreadHeader':bool(self.headers.get('thread-id')),'hasCacheKey':bool(data.get('prompt_cache_key')),'flags':[word for word in ('title','memory','compact','summariz','summaris') if word in request_text]})
        item={'id':'msg_resume','type':'message','role':'assistant','status':'completed','content':[{'type':'output_text','text':'isolated resume seed complete','annotations':[]}]}
        self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
        try:
            for name,obj in [('response.created',{'response':{'id':'resp_resume','object':'response','status':'in_progress','output':[]}}),('response.output_item.added',{'output_index':0,'item':item}),('response.output_text.delta',{'item_id':'msg_resume','output_index':0,'content_index':0,'delta':'isolated resume seed complete'}),('response.output_item.done',{'output_index':0,'item':item}),('response.completed',{'response':{'id':'resp_resume','object':'response','status':'completed','output':[item],'usage':{'input_tokens':1,'output_tokens':1,'total_tokens':2}}})]:
                self.wfile.write(('event: '+name+'\ndata: '+json.dumps(dict(type=name,**obj))+'\n\n').encode());self.wfile.flush()
        except (BrokenPipeError,ConnectionResetError):pass
def history(ws,thread):
    turns=ws.call('thread/turns/list',{'threadId':thread,'limit':100})['result']['data'];items=[]
    for turn in turns:
        cursor=None
        while True:
            args={'threadId':thread,'turnId':turn['id'],'sortDirection':'asc','limit':100}
            if cursor:args['cursor']=cursor
            result=ws.call('thread/items/list',args)['result'];items+=result['data'];cursor=result.get('nextCursor')
            if not cursor:break
    return len(turns),hashlib.sha256(json.dumps([turns,items],sort_keys=True).encode()).hexdigest()
def start_tui(root,env,project,thread=None,form="launcher"):
    master,slave=pty.openpty();fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',36,140,0,0))
    flags=['--no-alt-screen']
    if thread is None:flags+=['--sandbox','danger-full-access','--ask-for-approval','never']
    if thread is None:args=['codex',*flags]
    elif form=='direct-env':args=['codex','resume',thread,'--remote','unix://'+env.get('CODEX_HOME',env['HOME']+'/.codex')+'/app-server-control/app-server-control.sock',*flags,'-a','never','-s','danger-full-access']
    elif form in ('plain','plain-overrides'):args=['codex','resume',thread,*flags,*(['-a','never','-s','danger-full-access'] if form=='plain-overrides' else [])]
    elif form.startswith('remote-'):
        extra={'remote-a':['-a','never'],'remote-long-a':['--ask-for-approval','never'],'remote-s':['-s','danger-full-access'],'remote-long-s':['--sandbox','danger-full-access'],'remote-bypass':['--dangerously-bypass-approvals-and-sandbox'],'remote-full-auto':['--full-auto'],'remote-review':['--approve-for-me'],'remote-config':[],'remote-pair':['-a','never','-s','danger-full-access'],'remote-pair-last':['-a','never','-s','danger-full-access'],'remote-long-pair':['--sandbox','danger-full-access','--ask-for-approval','never'],'remote-long-pair-last':['--sandbox','danger-full-access','--ask-for-approval','never']}[form]
        endpoint_path=Path(env['CODEX_HOME'])/'app-server-control/app-server-control.sock'
        if os.getenv('ASB_DAEMON_RESUME_DIAG')=='flags-canonical':endpoint_path=endpoint_path.resolve()
        endpoint='unix://'+str(endpoint_path)
        args=['codex','resume',thread,*flags,*extra,'--remote',endpoint] if form in ('remote-pair-last','remote-long-pair-last') else ['codex','resume',thread,'--remote',endpoint,*flags,*extra]
    elif form in ('probe','remote') or form.startswith('remote-'):
        args=['codex','resume',thread,'--remote','unix://'+str(Path(env['CODEX_HOME'])/'app-server-control/app-server-control.sock'),*(['--dangerously-bypass-hook-trust'] if form=='probe' else []),*flags]
    else:args=['sh',cfg['launcher'],thread,*flags,'-a','never','-s','danger-full-access']
    p=subprocess.Popen(args,cwd=project,env=env,stdin=slave,stdout=slave,stderr=slave,start_new_session=True);os.close(slave);stop=threading.Event();uis.append((p,master,stop,env,project))
    def drain():
        trusted=False;buffer=bytearray();p.probe_buffer=buffer
        while not stop.is_set():
            try:
                if select.select([master],[],[],.1)[0]:
                    data=os.read(master,65536)
                    if not data:return
                    if b'\x1b[6n' in data:os.write(master,b'\x1b[1;1R')
                    buffer.extend(data);del buffer[:-16000];clean=re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',buffer.decode(errors='ignore')).lower()
                    if ('hooks need review' in clean or 'hooks are new or changed' in clean) and not trusted:
                        trusted=True;time.sleep(.5);os.write(master,b'2\r')
            except OSError:return
    threading.Thread(target=drain,daemon=True).start();return master

def stop_hosts():
    # Record only task-owned processes while they still hold files under this fixture root.
    owned={int(x) for x in subprocess.run(['lsof','-t','+D',str(base)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True).stdout.split()}-{os.getpid()}
    for ws in controllers:
        try:ws.close()
        except OSError:pass
    controllers.clear()
    hosts=list(uis);uis.clear()
    for p,fd,stop,env,project in hosts:
        stop.set()
        if p.poll() is None:
            try:os.killpg(p.pid,signal.SIGTERM)
            except ProcessLookupError:pass
        try:p.wait(timeout=5)
        except subprocess.TimeoutExpired:p.kill();p.wait(timeout=5)
        try:os.close(fd)
        except OSError:pass
        try:subprocess.run(['codex','app-server','daemon','stop'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
        except subprocess.TimeoutExpired:emit('owned-daemon-stop-timeout')
    uis.clear()
    for _ in range(3):
        pids=owned & {int(x) for x in subprocess.run(['lsof','-t','+D',str(base)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True).stdout.split()}
        if not pids:break
        for pid in pids:
            try:os.kill(pid,signal.SIGTERM)
            except ProcessLookupError:pass
        time.sleep(.3)
def run_case(label,custom=False,stored=True,variant=None):
    global bridge,model,calls,current
    root=base/label;home=root/'h';ch=root/'custom' if custom else home/'.codex';project=root/'p';bin_dir=root/'bin'
    if variant=='long':ch=root/('c'*max(1,100-len(str(root))-1-len('/app-server-control/app-server-control.sock')))
    for path in (ch,project,root/'state',bin_dir):path.mkdir(parents=True,exist_ok=True)
    env={k:v for k,v in os.environ.items() if k in ('PATH','LANG','SDKROOT','SHELL')};env.update(HOME=str(home),CODEX_HOME=str(ch),TMPDIR='/private/tmp',TERM='xterm-256color',AGENT_SESSION_BROKER_STATE_DIR=str(root/'state'))
    # Resolve this candidate locally; no npm download, publication or global install.
    shim=bin_dir/'npx';shim.write_text('#!/usr/bin/env python3\nimport os,sys\nos.execv('+repr(cfg['binary'])+',['+repr(cfg['binary'])+']+sys.argv[3:])\n');shim.chmod(0o700);env['PATH']=str(bin_dir)+os.pathsep+env['PATH']
    current=dict(cfg,name='resume-'+label,phase='seed');calls=[];case_event_start=len(model_events)
    bridge=socket.socket(socket.AF_UNIX);bridge.bind(str(root/'bridge.sock'));bridge.listen();os.chmod(root/'bridge.sock',0o600)
    listener=bridge
    def bridge_loop():
        while True:
            try:
                c,_=listener.accept()
                with c:c.sendall(json.dumps(current).encode())
            except OSError:return
    threading.Thread(target=bridge_loop,daemon=True).start()
    model=ThreadingHTTPServer(('127.0.0.1',0),Model);threading.Thread(target=model.serve_forever,daemon=True).start()
    text='''approval_policy = "never"
sandbox_mode = "danger-full-access"
model = "gate"
model_provider = "gate"
[model_providers.gate]
name = "Local resume fixture"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
[mcp_servers.x-broker]
command = "/usr/bin/python3"
args = [%s, "mcp", %s]
'''%(model.server_port,json.dumps(str(SELF)),json.dumps(str(root)))
    if os.getenv('ASB_DAEMON_RESUME_DIAG') in ('flags-default','flags-pair','flags-long'):text=text.replace('approval_policy = "never"\nsandbox_mode = "danger-full-access"\n','')
    if custom:text+='[mcp_servers.x-broker.env]\nCODEX_HOME = '+json.dumps(str(ch))+'\nHOME = '+json.dumps(str(home))+'\n'
    text+='[projects.'+json.dumps(str(project))+']\ntrust_level = "trusted"\n';(ch/'config.toml').write_text(text)
    ingress={'type':'mcp_tool','server':'x-broker','tool':'ingress_hook','input':{'session_id':'${session_id}','hook_event_name':'${hook_event_name}','turn_id':'${turn_id}'},'timeout':10}
    hooks={'UserPromptSubmit':[{'hooks':[ingress]}]}
    if stored:hooks['UserPromptSubmit'][0]['hooks'].insert(0,{'type':'mcp_tool','server':'x-broker','tool':'attach_session','input':{},'timeout':10})
    (ch/'hooks.json').write_text(json.dumps({'hooks':hooks}))
    subprocess.run(['git','init','-q',str(project)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    fd=start_tui(root,env,project)
    wait('seed-MCP',lambda:rows(root,'initialized','seed'));time.sleep(.7)
    os.write(fd,b'Seed this isolated resume fixture.');time.sleep(.6);os.write(fd,b'\r')
    wait('seed-model',lambda:calls);thread=calls[0]
    socket_path=ch/'app-server-control/app-server-control.sock';wait('seed-socket',socket_path.exists);ws=WS(socket_path);controllers.append(ws)
    wait('seed-complete',lambda:ws.call('thread/turns/list',{'threadId':thread,'limit':1})['result']['data'][0]['status']=='completed')
    if stored:wait('seed-ready',lambda:roster(thread) and roster(thread).get('automaticDeliveryReady'))
    else:assert roster(thread) is None,'fresh-seed-registered'
    if variant=='lifecycle':
        misuse_lifecycle(root,env,project,thread,socket_path)
        stop_hosts();bridge.close();bridge=None;model.shutdown();model.server_close();model=None
        return
    before_history=history(ws,thread);seed_models=len(calls);before_holder=holder(thread) if stored else {'generation':0,'holderPID':0}
    stop_hosts()
    if stored:wait('old-lease-not-ready',lambda:roster(thread) and not roster(thread).get('automaticDeliveryReady'),40)
    hooks={'UserPromptSubmit':[{'hooks':[ingress]}]};(ch/'hooks.json').write_text(json.dumps({'hooks':hooks}))
    # Daemon shutdown can finish a seed title request; the resume window starts after it.
    before_models=len(calls);current['phase']='resume';before_counts=api('counters');started=time.monotonic()
    if os.getenv('ASB_DAEMON_RESUME_DIAG'):
        for form in (('remote-long-pair','remote-long-pair-last','launcher-overrides') if os.getenv('ASB_DAEMON_RESUME_DIAG')=='flags-long' else ('remote-pair','remote-pair-last','plain-overrides','remote-config') if os.getenv('ASB_DAEMON_RESUME_DIAG')=='flags-pair' else ('remote-a','remote-long-a','remote-s','remote-long-s','remote-bypass','remote-full-auto','remote-review','plain-overrides','remote-config') if os.getenv('ASB_DAEMON_RESUME_DIAG','').startswith('flags') else ('plain','probe','remote','launcher')):
            if form in ('probe','remote') or form.startswith('remote-'):
                subprocess.run(['codex','app-server','daemon','start'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=10)
            current['phase']=form;start_tui(root,env,project,thread,form)
            time.sleep(3)
            process=uis[-1][0];loaded=False;statuses=[];connected=False;policies={}
            try:
                observer=WS(socket_path);controllers.append(observer);connected=True
                loaded=thread in observer.call('thread/loaded/list',{}).get('result',{}).get('data',[])
                response=observer.call('mcpServerStatus/list',{'threadId':thread,'serverName':'x-broker','detail':'toolsAndAuthOnly'})
                statuses=[{'name':x.get('name'),'status':x.get('runtimeStatus')} for x in response.get('result',{}).get('data',[])]
                if form=='remote-config':
                    result=observer.call('config/read',{}).get('result',{}).get('config',{})
                    policies={key:result.get(key) for key in ('approval_policy','sandbox_mode')}
            except (OSError,RuntimeError):pass
            clean=re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',bytes(getattr(process,'probe_buffer',b'')).decode(errors='ignore')).lower()
            error_lines=[]
            for line in clean.splitlines():
                if 'error:' in line:
                    safe=re.sub(r'asb_[psu]_[A-Za-z0-9_-]+','<redacted>',line)
                    safe=re.sub(r'(?:https?|unix)://[^\s]+','<endpoint>',safe)
                    safe=re.sub(r'/[^\s:]+','<path>',safe)
                    error_lines.append(safe[:300])
            emit('tui-error',form=form,lines=error_lines[:2])
            emit('resume-diagnosis',form=form,socketSymlink=socket_path.is_symlink(),canonicalDiffers=str(socket_path)!=str(socket_path.resolve()),policies=policies,tuiPID=process.pid,mcpHostParents=[r.get('hostParentPID') for r in rows(root,'native-start',form)],tuiStarted=True,tuiExit=process.poll(),socketConnected=connected,loaded=loaded,statuses=statuses,nativeSpawned=bool(rows(root,'native-start',form)),uiFlags=[x for x in ('error','unexpected argument','hooks need review','failed to connect','session not found','permission denied') if x in clean])
            stop_hosts()
        return
    resume_env=dict(env)
    if variant in ('symlink','symlink-direct'):
        alias=root/'home alias';alias.symlink_to(ch,target_is_directory=True);resume_env['CODEX_HOME']=str(alias)
    if variant=='home-symlink':
        alias=root/'home alias';alias.symlink_to(home,target_is_directory=True);resume_env['HOME']=str(alias);resume_env.pop('CODEX_HOME',None)
    if variant=='relative':resume_env['CODEX_HOME']=os.path.relpath(ch,project)
    start_tui(root,resume_env,project,thread,form='direct-env' if variant=='symlink-direct' else 'launcher')
    if variant=='double':start_tui(root,resume_env,project,thread)
    if variant in ('relative','symlink','symlink-direct','home-symlink'):
        wait('home-outcome',lambda:rows(root,'native-start','resume') or uis[-1][0].poll() is not None)
        if not rows(root,'native-start','resume'):
            assert api('counters')==before_counts,'relative-refusal-registered'
            assert not rows(root,'host-tool','resume'),'relative-refusal-tool-call'
            emit('resume-safe-refusal',case=label,tuiExit=uis[-1][0].poll(),otherThreadCalls=0,uiFlags=[x for x in ('permission overrides are not supported','codex_home','invalid','error') if x in bytes(getattr(uis[-1][0],'probe_buffer',b'')).decode(errors='ignore').lower()])
            stop_hosts();bridge.close();bridge=None;model.shutdown();model.server_close();model=None
            return
    native=wait('resume-native',lambda:next(iter(rows(root,'native-start','resume')),None))['pid']
    if stored and variant=='home-symlink':
        wait('home-symlink-legacy-ready',lambda:roster(thread) and roster(thread).get('automaticDeliveryReady') and rows(root,'status','resume'))
        status=rows(root,'status','resume')[-1]
        assert status.get('registered') and status.get('receiverReady'),'home-symlink-receiver'
        assert status.get('deviceIdentity')=='directory-insecure','home-symlink-device-cause'
        assert holder(thread).get('holderPID')==0 and holder(thread).get('generation',0)>before_holder['generation'],'home-symlink-legacy-holder'
        assert all(e.get('thread')==thread for e in rows(root,'host-tool','resume')),'home-symlink-wrong-thread'
        assert api('counters')==before_counts,'home-symlink-new-registration'
        emit('home-symlink-design',deviceIdentity='directory-insecure',legacyHolderless=True,receiverReady=True,otherThreadCalls=0)
    elif stored:
        def owner_ready():
            row=roster(thread);owner=holder(thread)
            return bool(row and row.get('automaticDeliveryReady') and owner['holderPID']==native and owner['generation']>before_holder['generation'])
        try:wait('strict-current-owner-ready',owner_ready)
        except RuntimeError:
            row=roster(thread);owner=holder(thread)
            emit('owner-diagnostic',case=label,holderPID=owner.get('holderPID'),nativePIDs=[e['pid'] for e in rows(root,'native-start','resume')],authoritativePIDs=[e['pid'] for e in rows(root,'host-tool','resume') if e.get('tool')=='auth_status' and e.get('thread')==thread],brokerEvents=[{k:e.get(k) for k in ('tool','holderPID','generation','ok','code')} for e in api('events') if e.get('thread')==thread],nativeCount=len(rows(root,'native-start','resume')),holderAmongOwnedNatives=owner.get('holderPID') in {e['pid'] for e in rows(root,'native-start','resume')},holderMatchesAuthoritativeU=owner.get('holderPID') in {e['pid'] for e in rows(root,'host-tool','resume') if e.get('tool')=='auth_status' and e.get('thread')==thread},ready=bool(row and row.get('automaticDeliveryReady')),nativeIsHolder=owner.get('holderPID')==native,newGeneration=owner.get('generation',0)>before_holder['generation'],authCalls=len([e for e in rows(root,'host-tool','resume') if e.get('tool')=='auth_status']),statuses=[{k:e.get(k) for k in ('registered','receiverReady','status')} for e in rows(root,'status','resume')])
            raise
        wait('receiverReady',lambda:any(e.get('receiverReady') is True for e in rows(root,'status','resume')))
    else:
        wait('fresh-auth-status',lambda:rows(root,'status','resume'));time.sleep(.7)
        assert not any(e.get('registered') for e in rows(root,'status','resume')),'fresh-registered'
        assert roster(thread) is None,'fresh-roster-entry'
        assert api('counters')==before_counts,'fresh-registration-or-issuance'
        assert len([e for e in rows(root,'host-tool','resume') if e['tool']=='auth_status'])==1,'fresh-auth-retried'
    wait('resume-socket',socket_path.exists);after=WS(socket_path);controllers.append(after);after_history=history(after,thread)
    assert before_history==after_history,'history-changed'
    if len(calls)!=before_models:emit('model-request-diagnosis',case=label,seedCount=before_models,afterCount=len(calls),events=[{'phase':e['phase'],'sameThread':e['thread']==thread,'hasThreadHeader':e['hasThreadHeader'],'hasCacheKey':e['hasCacheKey'],'flags':e['flags'],'relativeMs':round((e['at']-started)*1000)} for e in model_events[case_event_start:]])
    assert len(calls)==before_models,'resume-model-request'
    if stored:assert native!=before_holder['holderPID'],'old-native-holder'
    if variant=='double':
        assert len({r['pid'] for r in rows(root,'native-start','resume')})==1,'duplicate-launcher-native'
        assert holder(thread)['generation']>before_holder['generation'],'duplicate-launcher-generation'
        wait('both-launcher-auth',lambda:len(rows(root,'status','resume'))>=2)
    emit('resume-pass',case=label,variant=variant,socketAliasBytes=len(str(socket_path).encode()),customHome=custom,stored=stored,latencyMs=round((time.monotonic()-started)*1000),currentNativeHolder=stored and variant!='home-symlink',legacyHolderless=variant=='home-symlink',modelRequests=0,seedTailRequests=before_models-seed_models,newTurns=0,historyUnchanged=True,authCalls=len([e for e in rows(root,'host-tool','resume') if e['tool']=='auth_status']))
    stop_hosts();bridge.close();bridge=None;model.shutdown();model.server_close();model=None
def misuse_lifecycle(root,env,project,thread,socket_path):
    unknown='99999999-9999-4999-8999-999999999999'
    own=[]
    def helper():
        p=subprocess.Popen([cfg['binary'],'codex-resume-helper','--thread',unknown,'--pid',str(uis[-1][0].pid),'--codex-home',env['CODEX_HOME'],'--socket',str(socket_path),'--inode',str(socket_path.stat().st_ino)],cwd=project,env=env,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        own.append(p);time.sleep(.7);assert p.poll() is None,'unloaded-helper-ended-early';return p
    try:
        p=helper();tui=uis[-1][0];os.killpg(tui.pid,signal.SIGTERM);tui.wait(timeout=5)
        assert p.wait(timeout=4)==1,'dead-tui-helper-success'
        emit('misuse-lifecycle',case='unloaded-and-tui-exit',unknownThreadCalls=0,helperExited=True)
        start_tui(root,env,project,thread);time.sleep(1)
        p=helper()
        subprocess.run(['codex','app-server','daemon','stop'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
        assert p.wait(timeout=4)==1,'stopped-daemon-helper-success'
        assert not any(r.get('thread')==unknown for r in rows(root,'host-tool')),'unknown-thread-tool-call'
        emit('misuse-lifecycle',case='daemon-stop-while-waiting',unknownThreadCalls=0,helperExited=True)
    finally:
        for p in own:
            if p.poll() is None:p.terminate();p.wait(timeout=5)

success=False
try:
    if os.getenv('ASB_MISUSE4'):
        variants={'lifecycle':False,'double':False,'symlink':True,'relative':True,'long':True,'symlink-direct':True,'home-symlink':False}
        selected=list(variants) if os.getenv('ASB_MISUSE4')=='1' else os.getenv('ASB_MISUSE4').split(',')
        for variant in selected:run_case(variant,custom=variants[variant],variant=variant)
    else:run_case('default')
    if not os.getenv('ASB_DAEMON_RESUME_DIAG') and not os.getenv('ASB_MISUSE4'):
        run_case('custom',custom=True)
        run_case('fresh',stored=False)
    success=True
except BaseException as exc:
    emit('failure',stage=str(exc) if isinstance(exc,(RuntimeError,AssertionError)) else type(exc).__name__)
finally:
    try:stop_hosts()
    except BaseException as exc:emit('cleanup-error',kind=type(exc).__name__,errno=getattr(exc,'errno',None))
    if bridge:bridge.close()
    if model:model.shutdown();model.server_close()
    remaining={int(x) for x in subprocess.run(['lsof','-t','+D',str(base)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True).stdout.split()}-{os.getpid()}
    emit('cleanup',remaining=len(remaining));shutil.rmtree(base)
sys.exit(0 if success and not remaining else 1)
