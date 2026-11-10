"""Build fixture-only tracing/reply loss without changing shipped source or checks."""
import json,subprocess,sys,tempfile
from pathlib import Path
source=Path(sys.argv[1]).resolve();binary=Path(sys.argv[2]).resolve();drop='--drop-reply' in sys.argv[3:]
target=source/'internal/channel/codex_control_posix.go'
text=target.read_text().replace('"errors"','"errors"\n "fmt"\n "os"\n "time"\n "sync/atomic"')
text=text.replace('type verifiedCodexControl', '''var fixtureSteerClientID atomic.Value
var fixtureDropped atomic.Bool
func fixtureSteerEvent(label string){fmt.Fprintf(os.Stderr,"STEER-FIXTURE:%s:%d\\n",label,time.Now().UnixNano())}
type verifiedCodexControl''',1)
trace='''label:="ambiguous"
 switch method {case "thread/turns/list":label="turns-list";case "thread/items/list":label="items-phase";case "turn/steer":label="steer-ok"}
 if err!=nil {kind,_:=steerRejection(err);label="steer-rejected:"+kind;if kind=="uncertain"{label="ambiguous"}}
 fixtureSteerEvent(label)
 return raw, err'''
if drop:
    trace='''if err==nil&&method=="turn/steer"&&!fixtureDropped.Load(){
  var accepted struct{TurnID string `json:"turnId"`}
  if json.Unmarshal(raw,&accepted)==nil&&accepted.TurnID!=""&&fixtureDropped.CompareAndSwap(false,true){
   id,_:=params.(map[string]any)["clientUserMessageId"].(string);fixtureSteerClientID.Store(id)
   fixtureSteerEvent("steer-accepted-dropped")
   return nil,context.DeadlineExceeded
  }
 }
 if err==nil&&method=="thread/items/list"{
  var page struct{Data []struct{Item struct{ClientID string `json:"clientId"`} `json:"item"`} `json:"data"`}
  id,_:=fixtureSteerClientID.Load().(string)
  if id!=""&&json.Unmarshal(raw,&page)==nil {for _,entry:=range page.Data{if entry.Item.ClientID==id {fixtureSteerEvent("positive-match");break}}}
 }
'''+trace
assert 'return raw, err' in text
text=text.replace('return raw, err',trace,1)
text=text.replace('if err != nil {\n\t\t\treturn nil, err','if err != nil {\n fixtureSteerEvent("verify-failed");return nil, err',1)
text=text.replace('if e != nil {\n\t\t\treturn nil, e','if e != nil {\n fixtureSteerEvent("verify-failed");return nil, e',1)
with tempfile.TemporaryDirectory(prefix='st-trace-',dir=binary.parent) as root:
    root=Path(root);replacement=root/'control.go';replacement.write_text(text)
    overlay=root/'overlay.json';overlay.write_text(json.dumps({'Replace':{str(target):str(replacement)}}))
    subprocess.run(['go','build','-overlay',str(overlay),'-ldflags','-X main.version=0.17.3','-o',str(binary),'./cmd/asb-client'],cwd=source,check=True)
