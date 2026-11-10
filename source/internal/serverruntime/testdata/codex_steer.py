"""Owned Codex daemon/product client E2E; outputs fixed labels and safe timings only."""
import base64,fcntl,json,os,pty,re,select,shutil,signal,socket,struct,subprocess,sys,tempfile,termios,threading,time,urllib.request
from pathlib import Path
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
SELF=Path(__file__).resolve()
def emit(label,**fields):print(json.dumps(dict(label=label,**fields)),flush=True)
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
    child=subprocess.Popen([cfg['binary'],'--broker-url',cfg['endpoint'],'--token',cfg['token'],'--state-dir',str(root/'state'),'--nickname',cfg['name'],'--codex-steer',cfg['steer']],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,env=dict(os.environ,ASB_STEER_CROSS_GATE=str(root/'cross-gate')))
    (root/'native-pid').write_text(str(child.pid))
    def stderr_pump():
        for raw in child.stderr:
            value=raw.decode(errors='ignore').strip()
            if re.fullmatch(r'X1DIAG [a-z0-9 =:-]+',value):
                with (root/'x1-diag').open('a') as f:f.write(value+'\n')
            if re.fullmatch(r'DEDUPE-DIAG [a-z0-9 =]+',value):
                with (root/'dedupe-diag').open('a') as f:f.write(value+'\n')
            match=re.fullmatch(r'STEER-FIXTURE:(verify-failed|turns-list|items-phase|steer-ok|steer-rejected:(?:retry|rejected|unsupported)|ambiguous|steer-accepted-dropped|positive-match):([0-9]{15,})',value)
            if match:
                with (root/'rpc-classes').open('a') as f:f.write(match[1]+'\n')
                with (root/'rpc-events').open('a') as f:f.write(json.dumps({'class':match[1],'at':int(match[2])/1e9})+'\n')
    threading.Thread(target=stderr_pump,daemon=True).start()
    (root/'mcp-started').touch()
    pending={};lock=threading.Lock()
    def input_pump():
        try:
            for line in sys.stdin.buffer:
                q=json.loads(line)
                if q.get('method')=='notifications/initialized':(root/'mcp-initialized').touch()
                if q.get('method')=='tools/call':
                    p=q.get('params',{});pending[q['id']]=p.get('name')
                    if p.get('name')=='ingress_hook':
                        event=p.get('arguments',{}).get('hook_event_name')
                        if event in ('PreToolUse','PostToolUse','UserPromptSubmit','Stop','Interrupt'):
                            with (root/'hook-events').open('a') as f:f.write(event+'\n')
                child.stdin.write(line);child.stdin.flush()
        except (OSError,BrokenPipeError):pass
        finally:
            try:child.stdin.close()
            except OSError:pass
    threading.Thread(target=input_pump,daemon=True).start()
    try:
        for line in child.stdout:
            q=json.loads(line);r=q.get('result',{});sc=r.get('structuredContent',{}) if isinstance(r,dict) else {};name=pending.pop(q.get('id'),None)
            if name=='ingress_hook' and 'STEER_E2E_' in json.dumps(sc):
                with (root/'hook-copies').open('a') as f:f.write('copy\n')
            if name=='auth_status' and isinstance(sc,dict):
                with (root/'status.json').open('w') as f:json.dump({k:sc.get(k) for k in ('registered','codexDelivery')},f)
            sys.stdout.buffer.write(line);sys.stdout.buffer.flush()
    except (OSError,BrokenPipeError):pass
    try:child.wait(timeout=5)
    except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=5)
    sys.exit()
if len(sys.argv)>1 and sys.argv[1]=='slow':
    root=Path(sys.argv[2])
    for line in sys.stdin:
        q=json.loads(line);method=q.get('method');result={}
        if method=='initialize':result={'protocolVersion':q['params']['protocolVersion'],'capabilities':{'tools':{}},'serverInfo':{'name':'slow','version':'1'}}
        elif method=='tools/list':result={'tools':[{'name':'hold','description':'An isolated timing fixture.','inputSchema':{'type':'object','properties':{}}}]}
        elif method=='tools/call':
            (root/'tool-start').touch();time.sleep(8);(root/'tool-done').touch();result={'content':[{'type':'text','text':'isolated command complete'}]}
        if 'id' in q:print(json.dumps({'jsonrpc':'2.0','id':q['id'],'result':result}),flush=True)
    sys.exit()
cfg=json.load(sys.stdin);base=Path(tempfile.mkdtemp(prefix='st-',dir='/private/tmp'));uis=[];model=None;bridge=None;current={};calls=[]
def wait(label,predicate,seconds=25):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        value=predicate()
        if value:return value
        time.sleep(.05)
    raise RuntimeError(label)
def api(path,data=None):
    req=urllib.request.Request(cfg['origin']+'/probe/'+path,data=json.dumps(data).encode() if data is not None else None,headers={'Content-Type':'application/json'})
    with urllib.request.urlopen(req,timeout=12) as response:return json.load(response)
def tool_template(tools):
    entries=[]
    for group in tools:
        if group.get('type')=='namespace':
            entries.extend((tool,group['name']) for tool in group.get('tools',[]))
        else:entries.append((group,None))
    if current.get('shell'):
        candidates=[]
        for tool,namespace in entries:
            keys=list(tool.get('parameters',{}).get('properties',{}))
            if ('cmd' in keys or 'command' in keys or re.search(r'shell|exec|command',tool.get('name',''))):
                emit('shell-declaration',name=tool.get('name'),type=tool.get('type'),namespace=namespace,parameterKeys=keys)
                if tool.get('type')=='function' and ('cmd' in keys or 'command' in keys):candidates.append((tool,namespace,keys))
        if not candidates:raise RuntimeError('declared-shell-function-missing')
        tool,namespace,keys=candidates[0]
        current['shell_parameter_keys']=keys
    else:
        candidate=next(((tool,ns) for tool,ns in entries if tool.get('name')=='hold' or ('slow' in tool.get('name','') and 'hold' in tool.get('name',''))),None)
        if not candidate:raise RuntimeError('declared-slow-tool-missing')
        tool,namespace=candidate
    result={'name':tool['name']}
    if namespace:result['namespace']=namespace
    return result
def request_kind(request,data):
    tools=data.get('tools',[])
    instructions=str(data.get('instructions',''))
    # Only test these fixed flags; never return or log the inspected text.
    inputs=data.get('input',[])
    text=instructions+' '+json.dumps(inputs)
    signals={k:bool(re.search(p,text,re.I)) for k,p in {
      'title':r'(?:generate|create|write|provide|return|produce).{0,100}\btitle\b',
      'summary':r'(?:summarize|summarise|compaction|compact the|generate.{0,40}summary)',
      'memory':r'(?:memory consolidation|consolidate memories)'}.items()}
    subagent=' '.join(v for k,v in request.headers.items() if 'subagent' in k.lower()).lower()
    if 'title' in subagent or (not tools and signals['title']):kind='aux-title'
    elif 'compact' in subagent or (not tools and signals['summary']):kind='aux-summary'
    elif 'memory' in subagent or (not tools and signals['memory']):kind='aux-memory'
    elif subagent:kind='aux-other'
    elif tools:kind='main-turn'
    else:kind='unclassified'
    roles={}
    for x in inputs if isinstance(inputs,list) else []:
        if isinstance(x,dict):
            role=x.get('role','no-role');role=role if role in ('system','developer','user','assistant') else 'other';roles[role]=roles.get(role,0)+1
    return kind,{'endpoint':request.path.split('?')[0],'tools':len(tools),'roles':roles,'signals':signals,'subagentHeaderPresent':bool(subagent)}

class Model(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def do_POST(self):
        data=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))));raw=json.dumps(data.get('input',[]));thread=self.headers.get('thread-id') or data.get('prompt_cache_key');mode=current.get('mode','plain');marker=current.get('marker','');has_marker=bool(marker and marker in raw)
        kind,shape=request_kind(self,data)
        if kind!='main-turn' or (current.get('targetThread') and thread!=current['targetThread']):mode='plain'
        call={'thread':thread,'requestKind':kind,'marker':has_marker,'at':time.monotonic(),'wall':time.time(),'markerID':marker if has_marker else None,'markerCount':raw.count(marker) if marker else 0,'restartReceiptCopies':raw.count('STEER_E2E_STOP_AMBIGUOUS'),'burstCounts':[raw.count(x) for x in current.get('burstMarkers',[])]};calls.append(call)
        self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
        def send(name,**obj):self.wfile.write(('event: '+name+'\ndata: '+json.dumps(dict(type=name,**obj))+'\n\n').encode());self.wfile.flush()
        try:
            send('response.created',response={'id':'resp_fixture','object':'response','status':'in_progress','output':[]})
            if mode=='tool' and not current.get('tool_issued'):
                template=tool_template(data.get('tools',[]))
                current['tool_issued']=True
                args={}
                if current.get('shell'):
                    import shlex
                    code="import pathlib,time; pathlib.Path(%r).touch(); time.sleep(8); pathlib.Path(%r).touch()"%(str(current['root']/'tool-start'),str(current['root']/'tool-done'))
                    keys=current['shell_parameter_keys'];args={'cmd' if 'cmd' in keys else 'command':shlex.join(['/usr/bin/python3','-c',code])}
                    if 'yield_time_ms' in keys:args['yield_time_ms']=10000
                    if 'timeout_ms' in keys:args['timeout_ms']=15000
                item=dict(type='function_call',id='fc_fixture',call_id='slow_fixture',arguments=json.dumps(args),**template)
                emit('tool-template',name=item['name'],namespace=item.get('namespace'))
                send('response.output_item.added',output_index=0,item=item);send('response.output_item.done',output_index=0,item=item)
            else:
                item={'id':'msg_fixture','type':'message','role':'assistant','status':'in_progress','content':[{'type':'output_text','text':'fixture complete','annotations':[]}]}
                send('response.output_item.added',output_index=0,item=item)
                # Keep generation genuinely active until steer or the explicit race release.
                if mode in ('generation','off','embedded') and not has_marker:
                    current['generating']=True
                    until=time.monotonic()+16
                    while time.monotonic()<until and not current.get('release'):
                        send('response.output_text.delta',item_id='msg_fixture',output_index=0,content_index=0,delta='progress ');time.sleep(.1)
                if mode=='race' and not has_marker:time.sleep(.08)
                item['status']='completed'
                send('response.output_text.delta',item_id='msg_fixture',output_index=0,content_index=0,delta='fixture complete')
                send('response.output_item.done',output_index=0,item=item)
            send('response.completed',response={'id':'resp_fixture','object':'response','status':'completed','output':[item],'usage':{'input_tokens':1,'output_tokens':1,'total_tokens':2}})
        except (BrokenPipeError,ConnectionResetError):pass
        except BaseException as exc:emit('model-fixture-error',kind=type(exc).__name__,stage=str(exc) if str(exc) in ('declared-slow-tool-missing','declared-shell-function-missing') else 'fixture',requestKind=kind,toolCount=len(data.get('tools',[])))
def mcp(ws,thread,tool,args=None):
    q=ws.call('mcpServer/tool/call',{'threadId':thread,'server':'x-broker','tool':tool,'arguments':args or {}})
    assert 'result' in q and not q['result'].get('isError'),'mcp-tool-call'
    return q['result'].get('structuredContent',{})
def latest(ws,thread):return ws.call('thread/turns/list',{'threadId':thread,'limit':1})['result']['data']
def items(ws,thread,turn):
    out=[];cursor=None
    while True:
        args={'threadId':thread,'turnId':turn,'sortDirection':'asc','limit':100}
        if cursor:args['cursor']=cursor
        r=ws.call('thread/items/list',args)['result'];out+=[entry['item'] for entry in r['data'] if entry.get('turnId')==turn];cursor=r.get('nextCursor')
        if not cursor:return out
def diagnose(ws,thread,root):
    status=mcp(ws,thread,'auth_status').get('codexDelivery',{})
    safe={k:status.get(k) for k in ('mode','reason')}
    classes=(root/'rpc-classes').read_text().splitlines() if (root/'rpc-classes').exists() else []
    turns=latest(ws,thread)
    phase={}
    if turns:
        entries=items(ws,thread,turns[0]['id'])
        if entries:
            entry=entries[-1];nested=entry.get('item',{})
            allowed_types={'commandExecution','fileChange','mcpToolCall','dynamicToolCall','collabAgentToolCall','imageGeneration','agentMessage','userMessage','reasoning'}
            allowed_status={'inProgress','completed','failed','interrupted'}
            current['saw_in_progress_item']=current.get('saw_in_progress_item',False) or any(i.get('status')=='inProgress' and i.get('type') in allowed_types for i in entries)
            phase={'wrappedItem':isinstance(entry.get('item'),dict),'type':entry.get('type') if entry.get('type') in allowed_types else None,'status':entry.get('status') if entry.get('status') in allowed_status else None,'nestedType':nested.get('type') if nested.get('type') in allowed_types else None,'nestedStatus':nested.get('status') if nested.get('status') in allowed_status else None}
    snapshot=json.dumps([safe,classes,phase])
    if current.get('diagnostic')!=snapshot:
        current['diagnostic']=snapshot;emit('active-diagnostic',**safe,rpcClasses=classes,phase=phase)
def marker_count(ws,thread,turn,marker):return sum(i.get('type')=='userMessage' and marker in json.dumps(i) for i in items(ws,thread,turn))
def start_turn(ws,thread,text):
    q=ws.call('turn/start',{'threadId':thread,'input':[{'type':'text','text':text,'text_elements':[]}]});assert 'result' in q,'turn-start';return q['result']['turn']['id']
def stop_hosts():
    global bridge,model
    current['release']=True
    for p,fd,stop,env,project in uis:
        stop.set()
        try:os.killpg(p.pid,signal.SIGTERM)
        except ProcessLookupError:pass
        try:p.wait(timeout=5)
        except subprocess.TimeoutExpired:p.kill();p.wait(timeout=5)
        os.close(fd)
        try:subprocess.run(['codex','app-server','daemon','stop'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
        except subprocess.TimeoutExpired:emit('owned-daemon-stop-timeout')
    uis.clear()
    if bridge:bridge.close();bridge=None
    if model:model.shutdown();model.server_close();model=None
    for _ in range(3):
        pids={int(x) for x in subprocess.run(['lsof','-t','+D',str(base)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True).stdout.split()}-{os.getpid()}
        if not pids:break
        for pid in pids:
            try:os.kill(pid,signal.SIGTERM)
            except ProcessLookupError:pass
        time.sleep(.3)
def setup(label,embedded=False,off=False,name=None):
    global bridge,model,current,calls,root
    root=base/label;home=root/'h';ch=home/'.codex';project=root/'p'
    for p in (ch,project,root/'state'):p.mkdir(parents=True,exist_ok=True)
    env={k:v for k,v in os.environ.items() if k in ('PATH','LANG','SDKROOT','SHELL')};env.update(HOME=str(home),CODEX_HOME=str(ch),TMPDIR='/private/tmp',TERM='xterm-256color')
    current={};calls=[]
    local=dict(cfg,name=name or 'steer-'+label,steer='off' if off else 'on')
    if label in ('e','k','x3','r2b','dd-interrupt','dd-mcp-kill','r2b-guarded'):local['binary']=cfg['wrapped']
    bridge=socket.socket(socket.AF_UNIX);bridge.bind(str(root/'bridge.sock'));os.chmod(root/'bridge.sock',0o600);bridge.listen()
    listener=bridge
    def bridge_loop():
        while True:
            try:
                c,_=listener.accept()
                with c:c.sendall(json.dumps(local).encode())
            except OSError:return
    threading.Thread(target=bridge_loop,daemon=True).start()
    model=ThreadingHTTPServer(('127.0.0.1',0),Model);threading.Thread(target=model.serve_forever,daemon=True).start()
    (ch/'config.toml').write_text('''model = "gate"
model_provider = "gate"
[model_providers.gate]
name = "Isolated streaming fixture"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
[mcp_servers.x-broker]
command = "/usr/bin/python3"
args = [%s, "mcp", %s]
[mcp_servers.slow]
command = "/usr/bin/python3"
args = [%s, "slow", %s]
[projects.%s]
trust_level = "trusted"
'''%(model.server_port,json.dumps(str(SELF)),json.dumps(str(root)),json.dumps(str(SELF)),json.dumps(str(root)),json.dumps(str(project))))
    ingress={'type':'mcp_tool','server':'x-broker','tool':'ingress_hook','input':{'session_id':'${session_id}','hook_event_name':'${hook_event_name}','turn_id':'${turn_id}'},'timeout':10}
    hooks={e:[{'matcher':'*','hooks':[ingress]}] for e in ('UserPromptSubmit','PreToolUse','PostToolUse','Stop')}
    attach={'type':'mcp_tool','server':'x-broker','tool':'attach_session','input':{},'timeout':10}
    hooks['SessionStart']=[{'hooks':[attach]}]
    hooks['UserPromptSubmit'][0]['hooks'].insert(0,attach)
    (ch/'hooks.json').write_text(json.dumps({'hooks':hooks}))
    subprocess.run(['git','init','-q',str(project)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    master,slave=pty.openpty();fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',36,140,0,0))
    args=['codex','--no-alt-screen','--sandbox','danger-full-access','--ask-for-approval','never']
    if embedded:args+=['-c','model_reasoning_effort="low"','--dangerously-bypass-hook-trust']
    p=subprocess.Popen(args,cwd=project,env=env,stdin=slave,stdout=slave,stderr=slave,start_new_session=True);os.close(slave);stop=threading.Event();uis.append((p,master,stop,env,project))
    def drain():
        trusted=False;buffer=bytearray()
        while not stop.is_set():
            try:
                if select.select([master],[],[],.1)[0]:
                    data=os.read(master,65536)
                    if not data:return
                    if b'\x1b[6n' in data:os.write(master,b'\x1b[1;1R')
                    buffer.extend(data);del buffer[:-16000];clean=re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',buffer.decode(errors='ignore')).lower()
                    current['ui_flags']={'hookTrust':('hooks need review' in clean or 'hooks are new or changed' in clean),'mcpFailure':('mcp' in clean and 'failed' in clean),'readyEditor':('context left' in clean),'pressEnter':('press enter' in clean),'modelError':('model' in clean and ('error' in clean or 'not supported' in clean))}
                    if ('hooks need review' in clean or 'hooks are new or changed' in clean) and not trusted:trusted=True;time.sleep(.5);os.write(master,b'2\r')
            except OSError:return
    threading.Thread(target=drain,daemon=True).start()
    if embedded:wait('embedded-mcp-ready',lambda:(root/'mcp-initialized').exists())
    os.write(master,b'Initialize the isolated fixture.');time.sleep(.6);os.write(master,b'\r')
    wait('seed-model',lambda:calls)
    thread=calls[0]['thread']
    if embedded:
        wait('embedded-registration',lambda:next((r for r in api('roster') if r['name']==local['name'] and r.get('automaticDeliveryReady')),None))
        return root,thread,None
    ws=WS(ch/'app-server-control/app-server-control.sock')
    wait('seed-turn-complete',lambda:latest(ws,thread) and latest(ws,thread)[0]['status']!='inProgress')
    mcp(ws,thread,'attach_session')
    wait('mcp-registration',lambda:next((r for r in api('roster') if r['name']==local['name'] and r.get('automaticDeliveryReady')),None))
    status=mcp(ws,thread,'auth_status');assert status.get('registered'),'registered-status'
    return root,thread,ws
success=False
try:
    cases=(cfg.get('cases') or 'a,b,c,d,f,g').split(',')
    for label in cases:
        root,thread,ws=setup(label,embedded=label=='f',off=label=='g')
        if label=='f':
            current.update(mode='embedded',marker='STEER_E2E_F')
            # Embedded has no control socket; use its actual TUI prompt.
            master=uis[-1][1];os.write(master,b'Run the embedded delivery check.');time.sleep(.6);os.write(master,b'\r')
            wait('embedded-generation',lambda:current.get('generating'))
            sent=time.monotonic();assert api('send',{'to':'steer-f','body':'STEER_E2E_F'})['accepted'];time.sleep(.5)
            assert not (root/'h/.codex/app-server-control/app-server-control.sock').exists(),'embedded-socket'
            current['release']=True;wait('embedded-hook-delivery',lambda:any(c['marker'] for c in calls))
            emit('embedded-pass',steer=0)
        elif label=='c':
            marker='STEER_E2E_IDLE';current.update(mode='plain',marker=marker);assert api('send',{'to':'steer-c','body':marker})['accepted']
            wait('idle-queue-model',lambda:any(c['marker'] for c in calls));turn=latest(ws,thread)[0]['id'];assert marker_count(ws,thread,turn,marker)==1,'idle-count';emit('idle-pass',inputs=1)
        elif label=='d':
            for index in range(20):
                marker='STEER_E2E_RACE_'+str(index);current.clear();current.update(mode='race',marker=marker)
                turn=start_turn(ws,thread,'Finish the race fixture.')
                assert api('send',{'to':'steer-d','body':marker})['accepted'];wait('race-model',lambda:any(c.get('markerID')==marker for c in calls))
                wait('race-turn-finish',lambda:latest(ws,thread)[0]['status']!='inProgress')
                # Hook delivery is additionalContext rather than a native userMessage.
                samples=[c['markerCount'] for c in calls if c.get('markerID')==marker]
                assert samples and max(samples)==1,'race-duplicate-envelope'
                emit('race-case',index=index,received=True,maxEnvelopeCopies=max(samples))
            emit('race-pass',iterations=20)
        elif label=='h':
            markers=['MISUSE4_BURST_%03d'%n for n in range(16)]
            current.clear();current.update(mode='generation',marker=markers[0],burstMarkers=markers)
            turn=start_turn(ws,thread,'Exercise the isolated burst.')
            wait('burst-generating',lambda:current.get('generating'))
            registration=next(r['registrationId'] for r in api('roster') if r['name']=='steer-h')
            before=api('acks').get(registration,{}).get('through',0)
            for marker in markers:assert api('send',{'to':'steer-h','body':marker})['accepted'],'burst-send'
            wait('burst-ack',lambda:api('acks').get(registration,{}).get('through',0)>=before+len(markers))
            wait('burst-model',lambda:any(all(n==1 for n in c['burstCounts']) for c in calls if c['burstCounts']))
            wait('burst-finished',lambda:latest(ws,thread)[0]['status']!='inProgress')
            native=json.dumps([i for i in items(ws,thread,turn) if i.get('type')=='userMessage'])
            positions=[native.index(marker) for marker in markers]
            assert positions==sorted(positions) and all(native.count(marker)==1 for marker in markers),'burst-FIFO-or-duplicate'
            assert not (root/'hook-copies').exists(),'burst-hook-copy'
            emit('burst-pass',messages=16,FIFO=True,maxNativeCopies=1,hookCopies=0)
        elif label=='n':
            marker='MISUSE4_NO_ACK';current.clear();current.update(mode='generation',marker=marker)
            turn=start_turn(ws,thread,'Exercise legacy delivery.')
            wait('legacy-generation',lambda:current.get('generating'))
            assert api('send',{'to':'steer-n','body':marker})['accepted']
            time.sleep(.6);auth=mcp(ws,thread,'auth_status');status=auth.get('codexDelivery',{})
            emit('no-ack-diagnostic',server=auth.get('serverVersion'),mode=status.get('mode'),reason=status.get('reason'))
            assert auth.get('serverVersion')=='0.17.2','legacy-real-server-version'
            assert status.get('reason')=='ack-required','legacy-ACK-gate'
            assert marker_count(ws,thread,turn,marker)==0,'legacy-steer-input'
            current['release']=True;wait('legacy-hook-delivery',lambda:any(c['marker'] for c in calls))
            assert max(c['markerCount'] for c in calls if c['marker'])==1,'legacy-copy'
            emit('no-ack-pass',steer=0,reason='ack-required',hookCopies=1)
        elif label=='k':
            marker='MISUSE4_DAEMON_EXIT';current.clear();current.update(mode='tool',marker=marker,root=root)
            turn=start_turn(ws,thread,'Exercise daemon exit after acceptance.')
            wait('daemon-exit-tool',lambda:(root/'tool-start').exists())
            assert api('send',{'to':'steer-k','body':marker})['accepted']
            wait('daemon-exit-accepted',lambda:(root/'rpc-classes').exists() and 'steer-accepted-dropped' in (root/'rpc-classes').read_text())
            p,fd,stop,env,project=uis[-1]
            try:subprocess.run(['codex','app-server','daemon','stop'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
            except subprocess.TimeoutExpired:emit('daemon-stop-command-timeout')
            assert not (root/'hook-copies').exists(),'daemon-exit-hook-copy'
            classes=(root/'rpc-classes').read_text().splitlines()
            assert classes.count('steer-accepted-dropped')==1,'daemon-exit-resend'
            emit('daemon-exit-pass',accepted=1,resends=0,hookCopies=0,processRestartRecovery='documented-at-least-once-window')
        else:
            marker='STEER_E2E_'+label.upper();mode='tool' if label in ('a','a-shell','e') else 'off' if label=='g' else 'generation'
            current.clear();current.update(mode=mode,marker=marker,shell=label=='a-shell',root=root)
            turn=start_turn(ws,thread,'Exercise the isolated streaming fixture.')
            wait('tool-or-generation',lambda:(root/'tool-start').exists() if label in ('a','a-shell','e') else current.get('generating'))
            diagnose(ws,thread,root)
            registration=next(r['registrationId'] for r in api('roster') if r['name']=='steer-'+label)
            before=api('acks').get(registration,{}).get('through',0)
            sent=time.monotonic();sent_wall=time.time();assert api('send',{'to':'steer-'+label,'body':marker})['accepted']
            if label=='g':
                time.sleep(.5);assert marker_count(ws,thread,turn,marker)==0,'off-injected';current['release']=True
                wait('off-hook-delivery',lambda:any(c['marker'] for c in calls));emit('off-pass',steer=0)
            else:
                if label=='e':
                    wait('wrapped-drop',lambda:(root/'rpc-classes').exists() and 'steer-accepted-dropped' in (root/'rpc-classes').read_text())
                    other=WS(root/'h/.codex/app-server-control/app-server-control.sock')
                    for event in ('PostToolUse','Stop'):
                        out=mcp(other,thread,'ingress_hook',{'session_id':thread,'hook_event_name':event,'turn_id':turn});assert not out,'uncertain-hook-copy'
                    other.close()
                ack=wait('host-handoff-ack' ,lambda:api('acks').get(registration) if api('acks').get(registration,{}).get('through',0)>before else None)
                latency=ack['at']-sent_wall
                wait('native-input',lambda:(diagnose(ws,thread,root),marker_count(ws,thread,turn,marker)==1)[1])
                if label!='e':assert latency<1 if label in ('a','a-shell') else 9.5<=latency<14,'steer-latency'
                assert not (root/'hook-copies').exists(),'hook-copy'
                native=[i for i in items(ws,thread,turn) if i.get('type')=='userMessage' and marker in json.dumps(i)]
                assert len(native)==1 and native[0].get('clientId','').startswith('xb:'),'clientId-roundtrip'
                if label in ('a','a-shell','e'):wait('tool-survived',lambda:(root/'tool-done').exists())
                wait('same-turn-finish',lambda:latest(ws,thread)[0]['status']!='inProgress')
                assert latest(ws,thread)[0]['id']==turn,'same-turn'
                if label=='e':
                    events=[json.loads(line) for line in (root/'rpc-events').read_text().splitlines()]
                    matches=[event['at'] for event in events if event['class']=='positive-match']
                    assert matches and ack['at']>=min(matches),'ack-before-positive-match'
                    assert sum(event['class']=='steer-accepted-dropped' for event in events)==1,'wrapped-resend'
                    emit('wrapped-reply-loss-pass',ackAfterPositiveMatch=True,nativeInputs=1,hookCopies=0)
                nonce_calls=[c for c in calls if c['marker']]
                model_vs_tool_ms=round((nonce_calls[0]['wall']-(root/'tool-done').stat().st_mtime)*1000) if nonce_calls and (root/'tool-done').exists() else None
                emit('steer-pass',modelNonceVsToolEndMs=model_vs_tool_ms,case=label,latencyMs=round(latency*1000),inputs=1,hookCopies=0,clientId=True,sawInProgressItem=current.get('saw_in_progress_item',False),hookEvents=(root/'hook-events').read_text().splitlines() if (root/'hook-events').exists() else [])
        if ws:ws.close()
        stop_hosts()
    success=True
except BaseException as exc:
    emit('failure',stage=str(exc) if isinstance(exc,(RuntimeError,AssertionError)) else type(exc).__name__,mcpStarted=(root/'mcp-started').exists(),mcpInitialized=(root/'mcp-initialized').exists(),hookEvents=(root/'hook-events').read_text().splitlines() if (root/'hook-events').exists() else [],ui=current.get('ui_flags',{}))
finally:
    stop_hosts()
    remaining={int(x) for x in subprocess.run(['lsof','-t','+D',str(base)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True).stdout.split()}-{os.getpid()}
    emit('cleanup',remaining=len(remaining));shutil.rmtree(base)
sys.exit(0 if success and not remaining else 1)
