"""Round5 extends the existing owned real-host fixture; never emits config or UI."""
import ast
from pathlib import Path
original=Path(__file__).with_name('codex_steer.py')
__file__=str(original)
exec(compile(original.read_text().split('success=False\ntry:')[0],str(original),'exec'))
resume=original.with_name('codex_daemon_resume.py')
node=next(n for n in ast.parse(resume.read_text()).body if isinstance(n,ast.FunctionDef) and n.name=='start_tui')
exec(compile(ast.Module(body=[node],type_ignores=[]),str(resume),'exec'))
cfg['launcher']=str(Path(cfg['launcher']).resolve())
def mcp(ws,thread,tool,args=None):
    q=ws.call('mcpServer/tool/call',{'threadId':thread,'server':'x-broker','tool':tool,'arguments':args or {}})
    result=q.get('result',{});data=result.get('structuredContent',{})
    if 'error' in q or result.get('isError'):
        text=json.dumps(q).lower()
        emit('mcp-refusal',tool=tool,retiredReceiverMessage=('ingress registration was retired; retry registration after the receiver stops' in text),automaticRecoveringMessage=('automatic reauthorization is in progress' in text),codes=sorted(set(re.findall(r'[a-z]+(?:-[a-z]+)+',text)) & {'credential-rejected','automatic-reauthorization-stopped','registration-not-found','reattach-refused','identity-conflict','not-found','direct-registration-failed','authorization-pending','session-not-found','registration-in-use'}),flags=[k for k in ('stale','credential','pending','bound','registration','unauthorized','not found','device','permission','retired','detached') if k in text])
        raise RuntimeError('mcp-refused-'+tool)
    return data
def recovery_events(root,code):
    count=0
    for path in (root/'state/diagnostics/v2').glob('*.json'):
        try:
            events=json.loads(path.read_bytes()).get('events',[])
            count+=sum(e.get('event')==code for e in events)
        except (OSError,ValueError):pass
    return count
def roster(name):return next((r for r in api('roster') if r['name']==name),None)
def handoff(ws,u,name,label,hook=False):
    marker='STEER_E2E_CROSS_'+label
    current.clear();current.update(mode='generation' if hook else 'tool',marker=marker,root=root,targetThread=u)
    turn=start_turn(ws,u,'Exercise only the isolated cross-feature case.')
    wait(label+'-active',lambda:current.get('generating') if hook else (root/'tool-start').exists())
    assert api('send',{'to':name,'body':marker})['accepted'],label+'-send'
    if hook:current['release']=True
    wait(label+'-model',lambda:any(c.get('markerID')==marker for c in calls),35)
    wait(label+'-finished',lambda:latest(ws,u)[0]['status']!='inProgress')
    native=marker_count(ws,u,turn,marker);copies=max(c['markerCount'] for c in calls if c.get('markerID')==marker)
    assert copies==1 and native==(0 if hook else 1),label+'-duplicate-or-wrong-sink'
    emit(label,nativeInputs=native,maxModelEnvelopeCopies=copies)
    (root/'tool-start').unlink(missing_ok=True);(root/'tool-done').unlink(missing_ok=True)
def launcher_resume(ws,u,name):
    p,fd,stop,env,project=uis.pop();ws.close();stop.set();os.killpg(p.pid,signal.SIGTERM);p.wait(timeout=5);os.close(fd)
    try:subprocess.run(['codex','app-server','daemon','stop'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
    except subprocess.TimeoutExpired:emit('owned-daemon-stop-timeout')
    for _ in range(3):
        pids={int(x) for x in subprocess.run(['lsof','-t','+D',str(root)],capture_output=True,text=True).stdout.split()}-{os.getpid()}
        if not pids:break
        for pid in pids:
            try:os.kill(pid,signal.SIGTERM)
            except ProcessLookupError:pass
        time.sleep(.3)
    wait('old-not-ready',lambda:roster(name) and not roster(name).get('automaticDeliveryReady'),40)
    bindir=root/'bin';bindir.mkdir(exist_ok=True);shim=bindir/'npx';shim.write_text('#!/usr/bin/env python3\nimport os,sys\nos.execv('+repr(cfg['binary'])+',['+repr(cfg['binary'])+']+sys.argv[3:])\n');shim.chmod(0o700)
    env=dict(env,PATH=str(bindir)+os.pathsep+env['PATH'],AGENT_SESSION_BROKER_STATE_DIR=str(root/'state'))
    before=roster(name)['generation'];start_tui(root,env,project,u)
    wait('launcher-ready',lambda:roster(name) and roster(name).get('automaticDeliveryReady') and roster(name)['generation']>before)
    return WS(root/'h/.codex/app-server-control/app-server-control.sock')
class PendingSocket:
    def __init__(self,path):
        self.s=socket.socket(socket.AF_UNIX);self.s.settimeout(2);self.s.connect(str(path))
    def close(self):self.s.close()

def alive(pid):
    try:os.kill(pid,0);return True
    except ProcessLookupError:return False
def own_daemon_stop(ws,label):
    peer=ws.s.getsockopt(0,2) if sys.platform=='darwin' else struct.unpack('3i',ws.s.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))[0]
    env,project=uis[-1][3:];started=time.monotonic()
    if label=='r2b':ws.close()
    cmd=subprocess.Popen(['codex','app-server','daemon','stop'],cwd=project,env=env,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    timedout=False
    try:code=cmd.wait(timeout=12)
    except subprocess.TimeoutExpired:
        timedout=True;cmd.terminate()
        try:code=cmd.wait(timeout=2)
        except subprocess.TimeoutExpired:cmd.kill();code=cmd.wait(timeout=2)
    gone=not alive(peer)
    if label!='r2b':
        try:
            if isinstance(ws,PendingSocket):gone=ws.s.recv(1,socket.MSG_PEEK)==b''
            else:ws.call('thread/loaded/list',{},timeout=2)
        except (OSError,RuntimeError):gone=True
    emit('daemon-stop',case=label,elapsedMs=round((time.monotonic()-started)*1000),exit=code,timedOut=timedout,endpointGone=gone,peerPID=peer,peerAlive=alive(peer))
    if not gone and label=='r2b':
        # The recorded peer belongs to this fixture's private socket.
        os.kill(peer,signal.SIGKILL)
        wait('r2b-owned-peer-exit',lambda:not alive(peer),5)
        emit('daemon-owned-sigkill',peerPID=peer)
        gone=True
    assert gone,'r2-stop-kept-control-endpoint'
    return peer
def all_native(ws,u,marker):
    turns=ws.call('thread/turns/list',{'threadId':u,'limit':100})['result']['data']
    return sum(marker_count(ws,u,t['id'],marker) for t in turns)

def stop_hosts():
    global bridge,model
    current['release']=True
    for p,fd,stop,env,project in uis:
        stop.set()
        if p.poll() is None:p.terminate()
        try:p.wait(timeout=5)
        except subprocess.TimeoutExpired:p.kill();p.wait(timeout=5)
        try:os.close(fd)
        except OSError:pass
        try:subprocess.run(['codex','app-server','daemon','stop'],cwd=project,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
        except subprocess.TimeoutExpired:emit('owned-daemon-stop-timeout')
    uis.clear()
    if bridge:bridge.close();bridge=None
    if model:model.shutdown();model.server_close();model=None
    for _ in range(4):
        pids={int(x) for x in subprocess.run(['lsof','-t','+D',str(base)],capture_output=True,text=True).stdout.split()}-{os.getpid()}
        if not pids:break
        for pid in pids:
            try:os.kill(pid,signal.SIGTERM)
            except ProcessLookupError:pass
        time.sleep(.3)
resources=[]
success=False
try:
    for case in cfg.get('cases','x1,x3').split(','):
        root,u,ws=setup(case);name='steer-'+case
        resources.append((bridge,model))
        if case in ('x1','x1d'):
            ws=launcher_resume(ws,u,name);emit('x1-launcher-ready')
            marker='STEER_E2E_CROSS_STOP';current.clear();current.update(mode='tool',marker=marker,root=root)
            turn=start_turn(ws,u,'Exercise managed stop during steer.')
            wait('x1-tool',lambda:(root/'tool-start').exists())
            rid=roster(name)['registrationId'];assert api('send',{'to':name,'body':marker})['accepted']
            wait('x1-steer-accepted',lambda:marker_count(ws,u,turn,marker)==1);emit('x1-steer-accepted')
            if case=='x1d':assert api('recovery-arm',{'Registration':rid})['ok'],'x1d-arm'
            assert api('manage',{'Tool':'stop_session','Arguments':{'name':rid}})['ok'],'x1-stop'
            page=api('panel?name='+name);assert page['ok'] and not page['containsTarget'],'x1-panel-row'
            assert roster(name) is None,'x1-roster-cleaned';emit('x1-panel-cleaned')
            if case=='x1d':
                ws.call('mcpServer/tool/call',{'threadId':u,'server':'x-broker','tool':'get_session','arguments':{'name':rid}})
                wait('x1d-automatic-N1-in-flight',lambda:api('recovery-state')['waiting'])
                emit('x1d-recovery-barrier',automaticN1InFlight=True)
            if cfg.get('diagnose'):
                counts=api('recovery-state')
                until=time.monotonic()+60;resurrected=False;completion=False
                while time.monotonic()<until:
                    resurrected=resurrected or bool(roster(name))
                    events=(root/'x1-diag').read_text() if (root/'x1-diag').exists() else ''
                    if 'automatic-complete' in events:completion=True;break
                    time.sleep(.2)
                after_counts=api('recovery-state')
                assert after_counts['fresh']==counts['fresh'] and after_counts['registers']==counts['registers'],'x1-automatic-reissued'
                assert not resurrected,'x1-automatic-resurrection'
                emit('x1-automatic-observation',explicitAttachCalls=0,completion=completion,registrationReappeared=resurrected,freshRequests=0,registrations=0)
                mcp(ws,u,'attach_session')
                wait('x1-after60-ready',lambda:roster(name) and roster(name).get('automaticDeliveryReady'))
                emit('x1-explicit-attach-returned',ok=True,ready=True)
                if (root/'x1-diag').exists():emit('x1-fixed-diagnostics',events=(root/'x1-diag').read_text().splitlines())
                ws.close();stop_hosts();continue
            mcp(ws,u,'attach_session')
            wait('x1-reentry-ready',lambda:roster(name) and roster(name).get('automaticDeliveryReady'))
            if case=='x1d':
                barrier=api('recovery-state')
                assert barrier['disarmed'] and barrier['holds']==1,'x1d-barrier-not-one-shot'
                emit('x1d-barrier-released',holds=1,disarmed=True)
            wait('x1-stop-turn-finished',lambda:latest(ws,u)[0]['status']!='inProgress')
            (root/'tool-start').unlink(missing_ok=True);(root/'tool-done').unlink(missing_ok=True)
            handoff(ws,u,name,'x1-after-reattach-steer')
            emit('X1-pass',case=case,panelRowAbsent=True,sameName=True,oldMarkerNativeCopies=marker_count(ws,u,turn,marker))
        elif case=='x2':
            aws,au,aname=ws,u,name
            root,bu,bws=setup('x2b');resources.append((bridge,model));broot=root;bname='steer-x2b'
            b=roster(bname);mcp(aws,au,'get_session',{'name':bname})
            marker='STEER_E2E_CROSS_X2_BEFORE';current.clear();current.update(mode='tool',marker=marker,root=root)
            turn=start_turn(bws,bu,'Receive the isolated peer conversation.')
            wait('x2-b-tool',lambda:(root/'tool-start').exists())
            result=mcp(aws,au,'send_message',{'from':aname,'to':bname,'body':marker});assert result.get('accepted'),'x2-first-send'
            wait('x2-b-native',lambda:marker_count(bws,bu,turn,marker)==1)
            wait('x2-b-model',lambda:any(c.get('markerID')==marker for c in calls))
            wait('x2-b-finished',lambda:latest(bws,bu)[0]['status']!='inProgress')
            mcp(aws,au,'retire_session',{'registrationId':b['registrationId']});bws.close()
            for _ in range(4):
                pids={int(x) for x in subprocess.run(['lsof','-t','+D',str(broot)],capture_output=True,text=True).stdout.split()}-{os.getpid()}
                if not pids:break
                for pid in pids:
                    try:os.kill(pid,signal.SIGTERM)
                    except ProcessLookupError:pass
                time.sleep(.3)
            root,cu,cws=setup('x2c',embedded=True,name=bname);resources.append((bridge,model))
            assert cws is None and cu!=bu and roster(bname)['registrationId']!=b['registrationId'],'x2-new-identity'
            current.clear();marker='STEER_E2E_CROSS_X2_AFTER';current.update(mode='embedded',marker=marker)
            fd=uis[-1][1];os.write(fd,b'Wait for the isolated peer message.');time.sleep(.6);os.write(fd,b'\r')
            wait('x2-c-generation',lambda:current.get('generating'))
            reply=aws.call('mcpServer/tool/call',{'threadId':au,'server':'x-broker','tool':'send_message','arguments':{'from':aname,'to':bname,'body':'STEER_E2E_CROSS_X2_FORBIDDEN'}})
            payload=json.dumps(reply)
            assert 'recipient-changed' in payload,'x2-stale-name-not-refused'
            assert not roster(bname).get('pendingMessages',0),'x2-replacement-queue-not-empty'
            mcp(aws,au,'get_session',{'name':bname})
            assert mcp(aws,au,'send_message',{'from':aname,'to':bname,'body':marker}).get('accepted'),'x2-after-get-send'
            current['release']=True
            wait('x2-c-model',lambda:any(c.get('markerID')==marker and c['thread']==cu for c in calls))
            assert max(c['markerCount'] for c in calls if c.get('markerID')==marker)==1,'x2-copy'
            assert not any(c['thread']!=cu and c.get('markerID')==marker for c in calls),'x2-wrong-thread'
            emit('X2-pass',staleRefused=True,replacementQueue=0,embeddedHookCopies=1,wrongThread=0)
            aws.close();ws=None
        elif case=='x4':
            ws=launcher_resume(ws,u,name)
            before={r['id'] for r in api('roster')};fd=uis[-1][1]
            os.write(fd,b'/new');time.sleep(.8);os.write(fd,b'\r');time.sleep(.8)
            os.write(fd,b'Initialize this new isolated conversation.');time.sleep(.6);os.write(fd,b'\r')
            vrow=wait('x4-V-ready',lambda:next((r for r in api('roster') if r['id'] not in before and r.get('automaticDeliveryReady')),None))
            v=vrow['id'];assert v!=u,'x4-thread-same'
            handoff(ws,u,name,'x4-U')
            umarker='STEER_E2E_CROSS_x4-U'
            assert all(c['thread']==u for c in calls if c.get('markerID')==umarker),'x4-U-in-V'
            handoff(ws,v,vrow['name'],'x4-V')
            vmarker='STEER_E2E_CROSS_x4-V'
            assert all(c['thread']==v for c in calls if c.get('markerID')==vmarker),'x4-V-in-U'
            emit('X4-daemon-pass',sameDaemon=True,launcher=True,newConversation=True,steerU=1,steerV=1,wrongThread=0,yieldApplicable=False)
        elif case in ('dd-interrupt','dd-mcp-kill'):
            marker='STEER_E2E_'+case.upper().replace('-','_')
            current.clear();current.update(mode='tool',marker=marker,root=root,targetThread=u)
            turn=start_turn(ws,u,'Exercise isolated durable steer recovery.')
            wait('dedupe-tool',lambda:(root/'tool-start').exists())
            (root/'cross-gate.arm').touch()
            assert api('send',{'to':name,'body':marker})['accepted'],'dedupe-send'
            wait('dedupe-accepted-drop',lambda:(root/'cross-gate').exists())
            assert all_native(ws,u,marker)==0,'dedupe-expected-unobserved'
            if case=='dd-interrupt':
                reply=ws.call('turn/interrupt',{'threadId':u,'turnId':turn})
                assert 'result' in reply,'dedupe-interrupt'
                wait('old-tool-finished',lambda:(root/'tool-done').exists(),12)
                (root/'cross-gate.release').touch()
            else:
                pid=int((root/'native-pid').read_text())
                oldRegistration=roster(name)['registrationId'];oldGeneration=roster(name)['generation']
                daemonPeer=ws.s.getsockopt(0,2) if sys.platform=='darwin' else struct.unpack('3i',ws.s.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))[0]
                os.kill(pid,signal.SIGKILL)
                emit('dedupe-owned-mcp-killed',pid=pid)
                wait('receipt-visible-before-resume',lambda:all_native(ws,u,marker)==1,20)
                wait('accepted-turn-finished',lambda:latest(ws,u)[0]['status']!='inProgress')
                (root/'cross-gate.release').touch()
                reply=ws.call('config/mcpServer/reload',None)
                assert 'result' in reply,'positive-MCP-reload'
                mcp(ws,u,'auth_status')
                wait('positive-new-receiver-ready',lambda:roster(name) and roster(name).get('automaticDeliveryReady'))
                assert roster(name)['registrationId']==oldRegistration and roster(name)['generation']>oldGeneration,'positive-current-lease'
                assert alive(daemonPeer),'positive-daemon-survives'
            wait('dedupe-current-ACK',lambda:roster(name) and not roster(name).get('pendingMessages',0),40)
            if case=='dd-mcp-kill':
                wait('positive-receipt-ACK-diagnostic',lambda:recovery_events(root,'steer-recovery-acked')>0)
                assert int((root/'native-pid').read_text())!=pid,'positive-current-native-PID'
                current['mode']='plain'
                before=len(calls)
                observed=start_turn(ws,u,'Observe the recovered conversation without tools.')
                wait('positive-followup-main-model',lambda:any(c.get('markerID')==marker and c.get('requestKind')=='main-turn' for c in calls[before:]))
                wait('positive-followup-finished',lambda:latest(ws,u)[0]['id']==observed and latest(ws,u)[0]['status']!='inProgress')
            if case=='dd-interrupt':
                current['mode']='plain'
                wait('interrupted-thread-idle',lambda:latest(ws,u)[0]['status']!='inProgress')
                start_turn(ws,u,'Finish the isolated observation without tools.')
            wait('dedupe-model',lambda:any(c.get('markerID')==marker and c.get('requestKind')=='main-turn' for c in calls))
            wait('dedupe-finished',lambda:latest(ws,u)[0]['status']!='inProgress')
            assert all_native(ws,u,marker)==1,'dedupe-native-copy'
            assert max(c['markerCount'] for c in calls if c.get('markerID')==marker)==1,'dedupe-model-copy'
            rid=roster(name)['registrationId']
            wait('dedupe-ACK-through',lambda:api('acks').get(rid,{}).get('through',0)==1,15)
            emit('dedupe-ending-pass',ending=case,nativeInputs=1,modelEnvelopeCopies=1,ackThrough=1,redelivered=case=='dd-interrupt',positiveReceiptACK=case=='dd-mcp-kill',daemonUnchanged=case=='dd-mcp-kill')
        elif case in ('r2b','r2b-guarded'):
            ws=launcher_resume(ws,u,name)
            marker='STEER_E2E_STOP_AMBIGUOUS'
            current.clear();current.update(mode='tool',marker=marker,root=root,targetThread=u)
            turn=start_turn(ws,u,'Exercise the owned daemon death handoff.')
            wait('r2b-tool-in-flight',lambda:(root/'tool-start').exists())
            (root/'cross-gate.arm').touch()
            assert api('send',{'to':name,'body':marker})['accepted'],'r2b-send'
            wait('r2b-real-steer-accepted',lambda:(root/'cross-gate').exists())
            assert roster(name).get('pendingMessages',0)>0,'r2b-ACK-already-observed'
            emit('R2b-accepted-unobserved',nativeInputsBeforeStop=all_native(ws,u,marker),pending=True)
            if case=='r2b-guarded':
                peer=ws.s.getsockopt(0,2) if sys.platform=='darwin' else struct.unpack('3i',ws.s.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))[0]
                os.kill(peer,signal.SIGKILL);ws.close()
                wait('guarded-owned-peer-exit',lambda:not alive(peer),5)
                emit('guarded-owned-daemon-killed',peerPID=peer)
            else:own_daemon_stop(ws,case)
            (root/'cross-gate.release').touch()
            ws=launcher_resume(ws,u,name)
            restoredTurns=ws.call('thread/turns/list',{'threadId':u,'limit':100})['result']['data']
            restoredQueue=ws.call('thread/queue/list',{'threadId':u})['result']
            emit('R2b-restored-history',nativeInputs=all_native(ws,u,marker),targetPresent=any(t['id']==turn for t in restoredTurns),targetTerminal=all(t['status']!='inProgress' for t in restoredTurns),queueCount=len(restoredQueue.get('data',[])),queueMarker=marker in json.dumps(restoredQueue))
            wait('r2b-replayed-ACK',lambda:not roster(name).get('pendingMessages',0),40)
            rid=roster(name)['registrationId']
            wait('r2b-ACK-observed-on-renew',lambda:api('acks').get(rid,{}).get('through',0)>=1,15)
            wait('r2b-recovery-model-consumed',lambda:any(c.get('markerID')==marker and c.get('requestKind')=='main-turn' for c in calls))
            wait('r2b-recovery-receipt-visible',lambda:all_native(ws,u,marker)==1)
            wait('r2b-recovery-diagnostic',lambda:recovery_events(root,'steer-recovery-acked')+recovery_events(root,'steer-recovery-released')>0)
            recoveryKind='positive-ACK' if recovery_events(root,'steer-recovery-acked') else 'guarded-redelivery'
            if case=='r2b-guarded':assert recoveryKind=='guarded-redelivery','guarded-fixture-did-not-lose-receipt'
            count=all_native(ws,u,marker)
            copies=max([c.get('markerCount',0) for c in calls if c.get('markerID')==marker] or [0])
            rid=roster(name)['registrationId'];acks=api('acks')
            emit('R2b-recovery-outcome',nativeInputs=count,maxModelEnvelopeCopies=copies,ackThrough=acks.get(rid,{}).get('through',0),ready=bool(roster(name).get('automaticDeliveryReady')))
            assert count==1,'r2b-duplicate-after-daemon-restart'
            assert acks.get(rid,{}).get('through')==1,'r2b-current-ACK-prefix'
            wait('r2b-recovery-turn-finished',lambda:latest(ws,u)[0]['status']!='inProgress')
            (root/'tool-start').unlink(missing_ok=True);(root/'tool-done').unlink(missing_ok=True)
            handoff(ws,u,name,'r2b-new-message')
            wait('r2b-new-ACK',lambda:api('acks').get(rid,{}).get('through',0)>=2)
            copies=max(c.get('restartReceiptCopies',0) for c in calls)
            assert copies==1,'r2b-model-context-duplicate-or-loss'
            emit('R2b-strict-pass',nativeInputs=1,modelEnvelopeCopies=1,oldACK=1,newMessageProgress=True,recoveryKind=recoveryKind)
        elif case=='x3':
            marker='STEER_E2E_CROSS_RESTART';current.clear();current.update(mode='tool',marker=marker,root=root)
            turn=start_turn(ws,u,'Exercise persistent broker restart during active steer.')
            wait('x3-tool',lambda:(root/'tool-start').exists())
            assert api('send',{'to':name,'body':marker})['accepted']
            wait('x3-accepted-reply-lost',lambda:(root/'cross-gate').exists())
            wait('x3-uncertain',lambda:mcp(ws,u,'auth_status').get('codexDelivery',{}).get('reason')=='steer-uncertain')
            assert api('restart',{})['ok'],'x3-server-restart'
            wait('x3-reconnected',lambda:roster(name) and roster(name).get('automaticDeliveryReady'),45)
            for event in ('PostToolUse','Stop'):
                assert not mcp(ws,u,'ingress_hook',{'session_id':u,'hook_event_name':event,'turn_id':turn}),'x3-hook-copy'
            (root/'cross-gate-release').touch()
            wait('x3-model-input',lambda:any(c.get('markerID')==marker for c in calls),35)
            wait('x3-finish',lambda:latest(ws,u)[0]['status']!='inProgress')
            native=marker_count(ws,u,turn,marker);assert native==1,'x3-native-duplicate-or-loss'
            assert max(c['markerCount'] for c in calls if c.get('markerID')==marker)==1,'x3-model-duplicate'
            emit('X3-pass',nativeInputs=native,modelEnvelopeCopies=1,ambiguityInjected=True)
        if ws:ws.close()
        stop_hosts()
    success=True
except BaseException as exc:emit('failure',stage=str(exc) if isinstance(exc,(RuntimeError,AssertionError)) else type(exc).__name__)
finally:
    if (root/'dedupe-diag').exists():emit('dedupe-fixed-diagnostics',events=(root/'dedupe-diag').read_text().splitlines()[-120:])
    if (root/'x1-diag').exists():emit('x1-fixed-diagnostics',events=(root/'x1-diag').read_text().splitlines())
    stop_hosts()
    for b,m in resources:
        b.close();m.shutdown();m.server_close()
    remaining={int(x) for x in subprocess.run(['lsof','-t','+D',str(base)],capture_output=True,text=True).stdout.split()}-{os.getpid()}
    emit('cleanup',remaining=len(remaining));shutil.rmtree(base)
sys.exit(0 if success and not remaining else 1)
