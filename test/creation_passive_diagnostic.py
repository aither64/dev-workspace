
import datetime, json, math, pathlib, re, subprocess, sys, time, uuid
root = pathlib.Path('/tmp/creation-evidence')
failure = json.loads((root / 'browser-failure.json').read_text())
record = failure.get('record') or {}
slug, identity = record.get('slug', ""), record.get('threadId', "")
result = {'stage': 'after-failure-passive', 'slug': slug, 'threadId': identity, 'reads': []}
payloads = {}
exact = (re.fullmatch(r'[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9-]+', slug) and
         record.get('canonicalUrl') == '/' + slug + '/' and
         re.fullmatch(r'[0-9a-f-]{36}', identity) and str(uuid.UUID(identity)) == identity)
if exact:
    runtime, python, transport = sys.argv[1:]
    commands = [
        ('native', [python, transport, 'read', runtime + '/app-server.sock', identity]),
        ('portal', ['curl', '--silent', '--show-error', '--max-time', '8', '--unix-socket',
         runtime + '/portal.sock', '--write-out', '\n%{http_code}',
         'http://localhost/api/sessions/' + slug + '/thread/page'])]
    for kind, command in commands:
        reading = {'kind': kind, 'startedAt': datetime.datetime.now(datetime.timezone.utc).isoformat()}
        started = time.monotonic()
        try:
            process = subprocess.run(command, capture_output=True, text=True, timeout=8)
            reading.update(exit=process.returncode, stdoutBytes=len(process.stdout.encode()),
                           stderr=process.stderr[:500])
            output = process.stdout
            if kind == 'portal':
                output, _, reading['httpStatus'] = output.rpartition('\n')
            payload = json.loads(output)
            payloads[kind] = payload
            thread = payload['thread'] if kind == 'native' else payload
            entries = ([dict(item, turnId=turn['id'], turnStatus=turn['status'])
                        for turn in thread.get('turns', []) for item in turn.get('items', [])]
                       if kind == 'native' else thread.get('entries', []))
            reading.update(parsed=True, threadId=thread.get('id' if kind == 'native' else 'threadId'),
                           status=thread.get('status'), entryCount=len(entries),
                           initialCount=sum(failure['name'] and 'CREATION_INITIAL_' + failure['name'] in
                                            json.dumps(item) for item in entries
                                            if item.get('type', item.get('kind')) == 'userMessage'),
                           replyCount=sum(item.get('text') == 'CREATION_REPLY_' + record['provider']
                                          for item in entries
                                          if item.get('type', item.get('kind')) == 'agentMessage'),
                           entries=[{key: item.get(key) for key in ('type', 'kind', 'id', 'itemId',
                                    'turnId', 'turnStatus')} for item in entries[-24:]])
        except subprocess.TimeoutExpired:
            reading['timeoutSeconds'] = 8
        except Exception as error:
            reading.update(parsed=False, error=str(error)[:500])
        reading.update(endedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                       elapsedSeconds=time.monotonic() - started)
        result['reads'].append(reading)
else:
    result['skipped'] = 'exact failed browser identity unavailable'
# These are later passive observations, not the browser failure window.
def canonical_id(value):
    try:
        return value if isinstance(value, str) and str(uuid.UUID(value)) == value else None
    except ValueError:
        return None

def timestamp(value):
    if not isinstance(value, str) or len(value) > 64:
        return None
    try:
        parsed = datetime.datetime.fromisoformat(value)
        return value if parsed.tzinfo and re.fullmatch(r'[0-9T:.+Z-]+', value) else None
    except ValueError:
        return None

def elapsed(value):
    return value if type(value) in (int, float) and math.isfinite(value) and value >= 0 else None

provider = record.get('provider')
provider = provider if provider in ('primary', 'alternate') else None
name = failure.get('name')
marker = ('CREATION_INITIAL_' + name if isinstance(name, str) and
          re.fullmatch(r'[a-z0-9-]{1,256}', name) else None)
marker_pattern = (re.compile(r'(?<![A-Za-z0-9_-])' + re.escape(marker) + r'(?![A-Za-z0-9_-])')
                  if marker else None)
reply = 'CREATION_REPLY_' + provider if provider else None
readings = {item['kind']: item for item in result['reads']}

def observation(kind):
    reading = readings.get(kind, {})
    observed = dict(outcome='skipped' if not exact else 'unavailable',
                    exit=reading.get('exit') if type(reading.get('exit')) is int else None,
                    rootMatches=None, providerMatches=None,
                    entryCount=None, initialCount=None, initialTurnCount=None,
                    initialTurnStatus=None, replyInInitialTurnCount=None)
    if kind == 'portal':
        code = reading.get('httpStatus')
        observed.update(httpStatus=int(code) if isinstance(code, str) and
                        re.fullmatch(r'[0-9]{3}', code) else None, hasOlder=None)
    if not reading:
        return observed
    if 'timeoutSeconds' in reading:
        observed['outcome'] = 'timeout'
        return observed
    if type(observed['exit']) is not int or observed['exit'] != 0 or (kind == 'portal' and
            (observed['httpStatus'] is None or not 200 <= observed['httpStatus'] < 300)):
        observed['outcome'] = 'command-error'
        return observed
    if kind not in payloads:
        observed['outcome'] = 'invalid-json'
        return observed
    observed['outcome'] = 'invalid-shape'
    try:
        payload = payloads[kind]
        thread = payload['thread'] if kind == 'native' else payload
        returned = canonical_id(thread.get('id' if kind == 'native' else 'threadId'))
        if returned is None:
            return observed
        observed['rootMatches'] = returned == identity
        actual_provider = thread.get('modelProvider')
        observed['providerMatches'] = (actual_provider == provider
            if isinstance(actual_provider, str) and provider else None)
        if not observed['rootMatches'] or observed['providerMatches'] is False:
            observed['outcome'] = 'identity-mismatch'
            return observed
        if kind == 'portal':
            older = thread.get('hasOlder')
            observed['hasOlder'] = older if type(older) is bool else None
            entries = thread['entries']
        else:
            turns = thread['turns']
            if not isinstance(turns, list) or any(not isinstance(turn, dict) for turn in turns):
                return observed
            turn_ids = [turn['id'] for turn in turns]
            if any(not isinstance(value, str) or not value for value in turn_ids) or len(set(turn_ids)) != len(turn_ids):
                return observed
            if any(not isinstance(turn['items'], list) or
                   any(not isinstance(item, dict) for item in turn['items']) for turn in turns):
                return observed
            entries = [dict(item, turnId=turn['id'], turnStatus=turn.get('status'))
                       for turn in turns for item in turn['items']]
        if not isinstance(entries, list):
            return observed
        initials, replies, statuses = [], [], {}
        for item in entries:
            item_kind = item['type' if kind == 'native' else 'kind']
            turn_id = item['turnId']
            if not isinstance(item_kind, str) or not isinstance(turn_id, str) or not turn_id:
                return observed
            status = item.get('turnStatus')
            status = status if status in ('completed', 'inProgress', 'interrupted', 'failed') else None
            statuses.setdefault(turn_id, set()).add(status)
            if item_kind == 'userMessage':
                if kind == 'native':
                    content = item['content']
                    if not isinstance(content, list) or any(not isinstance(part, dict) or
                            not isinstance(part.get('type'), str) or
                            (part['type'] == 'text' and not isinstance(part.get('text'), str))
                            for part in content):
                        return observed
                    matches = marker_pattern is not None and any(marker_pattern.search(part['text'])
                        for part in content if part['type'] == 'text')
                else:
                    if not isinstance(item['text'], str):
                        return observed
                    matches = marker_pattern is not None and marker_pattern.search(item['text'])
                if matches:
                    initials.append(turn_id)
            elif item_kind == 'agentMessage':
                if not isinstance(item['text'], str):
                    return observed
                if reply is not None and item['text'] == reply:
                    replies.append(turn_id)
        if any(len(values) != 1 for values in statuses.values()):
            return observed
        observed.update(outcome='ok', entryCount=len(entries))
        if marker is not None:
            observed.update(initialCount=len(initials), initialTurnCount=len(set(initials)))
            if len(initials) == 1:
                observed['initialTurnStatus'] = next(iter(statuses[initials[0]]))
                if reply is not None:
                    observed['replyInInitialTurnCount'] = replies.count(initials[0])
    except (KeyError, TypeError, AttributeError, ValueError):
        pass
    return observed

try:
    summary = {'identity': {
        'state': 'valid' if exact else 'invalid',
        'requestId': canonical_id(record.get('requestId')),
        'receiptId': canonical_id(record.get('receiptId')),
        'threadId': canonical_id(identity),
        'slug': slug if isinstance(slug, str) and len(slug) <= 256 and
            re.fullmatch(r'[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9-]+', slug) and
            record.get('canonicalUrl') == '/' + slug + '/' else None,
        'provider': provider}, 'native': observation('native'), 'portal': observation('portal'),
        'timing': {'failureAt': timestamp(failure.get('at')), 'capturedAt': timestamp(failure.get('capturedAt'))}}
    for kind in ('native', 'portal'):
        reading = readings.get(kind, {})
        summary['timing'].update({kind + 'StartedAt': timestamp(reading.get('startedAt')),
            kind + 'EndedAt': timestamp(reading.get('endedAt')),
            kind + 'ElapsedSeconds': elapsed(reading.get('elapsedSeconds'))})
    line = json.dumps({'postFailurePassiveSummary': summary}, allow_nan=False)
    if len(line.encode()) > 8192:
        raise ValueError()
except Exception:
    line = json.dumps({'postFailurePassiveSummary': {'outcome': 'unavailable'}})
print(line, flush=True)
(root / 'post-failure-passive.json').write_text(json.dumps(result, indent=2))
