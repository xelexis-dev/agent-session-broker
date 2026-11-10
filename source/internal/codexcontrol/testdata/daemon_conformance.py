"""Owned plain-Codex daemon/MCP conformance. Emits only safe ids/statuses."""
import fcntl,json,os,pty,select,signal,struct,subprocess,sys,termios,threading,time
from pathlib import Path
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
config=json.loads(sys.stdin.readline());root=Path(config['root']);home=root/'home';ch=home/'.codex';project=root/'project'
for p in (ch,project):p.mkdir(parents=True,exist_ok=True)
env={k:v for k,v in os.environ.items() if k in ('PATH','LANG','SDKROOT','SHELL')};env.update(HOME=str(home),CODEX_HOME=str(ch),TMPDIR=os.getenv('TMPDIR','/tmp'),TERM='xterm-256color')
parent_file=root/'parent.json';calls=[];model_ready=threading.Event();tui=None;fd=None;stop=threading.Event();buffer=bytearray();started=time.time()
def wait(label,predicate,seconds=20):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        value=predicate()
        if value:return value
        time.sleep(.1)
    raise RuntimeError(label)
class Model(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def do_POST(self):
        d=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))));thread=self.headers.get('thread-id') or d.get('prompt_cache_key');calls.append(thread)
        item={'id':'msg_control','type':'message','role':'assistant','status':'completed','content':[{'type':'output_text','text':'control seed complete','annotations':[]}]}
        self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
        for name,data in [('response.created',{'response':{'id':'resp_control','object':'response','status':'in_progress','output':[]}}),('response.output_item.added',{'output_index':0,'item':item}),('response.output_text.delta',{'item_id':'msg_control','output_index':0,'content_index':0,'delta':'control seed complete'}),('response.output_item.done',{'output_index':0,'item':item}),('response.completed',{'response':{'id':'resp_control','object':'response','status':'completed','output':[item],'usage':{'input_tokens':1,'output_tokens':1,'total_tokens':2}}})]:
            try:self.wfile.write(('event: '+name+'\ndata: '+json.dumps(dict(type=name,**data))+'\n\n').encode());self.wfile.flush()
            except BrokenPipeError:break
        model_ready.set()
model=ThreadingHTTPServer(('127.0.0.1',0),Model);threading.Thread(target=model.serve_forever,daemon=True).start()
(ch/'config.toml').write_text('''model = "gate"
model_provider = "gate"
[model_providers.gate]
name = "Local control conformance"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
[mcp_servers.control-probe]
command = %s
[mcp_servers.control-probe.env]
ASB_CONTROL_TEST_HELPER = "1"
ASB_CONTROL_PARENT_FILE = %s
[projects.%s]
trust_level = "trusted"
'''%(model.server_port,json.dumps(config['binary']),json.dumps(str(parent_file)),json.dumps(str(project))))
subprocess.run(['git','init','-q',str(project)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
def emit(**d):print(json.dumps(d),flush=True)
try:
    master,slave=pty.openpty();fd=master;fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',36,140,0,0))
    # No -c, --enable/disable, --remote or --no-daemon: the TUI auto-starts its own daemon.
    tui=subprocess.Popen(['codex','--no-alt-screen','--sandbox','danger-full-access','--ask-for-approval','never'],cwd=project,env=env,stdin=slave,stdout=slave,stderr=slave,start_new_session=True);os.close(slave)
    def drain():
        while not stop.is_set():
            try:
                if select.select([fd],[],[],.1)[0]:
                    data=os.read(fd,65536)
                    if not data:break
                    buffer.extend(data)
                    if len(buffer)>65536:del buffer[:-65536]
                    if b'\x1b[6n' in data:os.write(fd,b'\x1b[1;1R')
            except OSError:break
    threading.Thread(target=drain,daemon=True).start()
    wait('auto-daemon-mcp',parent_file.exists)
    os.write(fd,b'CONTROL_SOCKET_SEED: say control seed complete.');time.sleep(.6);os.write(fd,b'\r')
    wait('local-model',model_ready.is_set);time.sleep(.5)
    parent=json.loads(parent_file.read_text());assert calls and isinstance(calls[0],str)
    emit(ok=True,parentPID=parent['parentPID'],mcpPID=parent['pid'],threadId=calls[0])
    sys.stdin.readline()
except BaseException as exc:
    emit(ok=False,stage=str(exc) if isinstance(exc,RuntimeError) else type(exc).__name__)
finally:
    stop.set()
    if tui:
        try:os.killpg(tui.pid,signal.SIGTERM)
        except ProcessLookupError:pass
        try:tui.wait(timeout=5)
        except subprocess.TimeoutExpired:pass
    if fd is not None:
        try:os.close(fd)
        except OSError:pass
    subprocess.run(['codex','app-server','daemon','stop'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
    model.shutdown()
    # Native daemon stop can leave an owned sidecar. Only the private test root
    # is queried, only PIDs are returned, and this controller is excluded.
    remaining=[]
    for attempt in range(3):
        pids={int(x) for x in subprocess.run(['lsof','-t','+D',str(root)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True).stdout.split()}-{os.getpid()}
        for pid in pids:
            try:os.kill(pid,signal.SIGTERM)
            except ProcessLookupError:pass
        time.sleep(.3)
        remaining=[int(x) for x in subprocess.run(['lsof','-t','+D',str(root)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True).stdout.split() if int(x)!=os.getpid()]
        if not remaining:break
    emit(ok=not remaining,remaining=len(remaining),models=len(calls))
