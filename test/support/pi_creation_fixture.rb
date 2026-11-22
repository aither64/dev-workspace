# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'time'

# Synthetic host-format evidence only. Native readiness/input proof is exercised
# through P3's typed fake verifier by the Go creation tests, never by this helper.
module PiCreationFixture
  def pi_ready_session_fixture(workspace, fork: false, env: {}, slug: 'example')
    root, authority = File.join(workspace, '.pi-state'), File.join(workspace, '.pi-authority')
    tmux = self.class::NullTmux.new
    tmux.define_singleton_method(:session) { |_slug| raise 'Pi must never query or adopt tmux' }
    runner = runner_for(workspace, tmux:, authority_dir: authority, env: {'DEV_WORKSPACES_STATE' => root}.merge(env))
    records = fork ? pi_fork_join_records(workspace, slug:) : pi_creation_records(workspace, slug:)
    slug = records[:intent]['slug']
    runner.ensure_tracking_files(slug)
    manifest = {'schema' => fork ? 5 : 4, 'slug' => slug, 'backend' => {'kind' => 'pi', 'contract' => 1, 'session_incarnation' => 'a' * 64},
      'creation' => {'state' => 'ready'}, 'repositories' => []}
    if fork
      manifest['forked_from'] = records[:intent]['fork']['source']['Slug']
    else
      manifest['creation'].merge!('goal_sha256' => records[:intent]['goal_sha256'], 'initial_goal_sent' => true)
    end
    runner.send(:write_portal_manifest, slug, manifest)
    stat = File.stat(File.join(workspace, 'work', slug))
    records[:completion].merge!('trackingDevice' => stat.dev, 'trackingInode' => stat.ino)
    paths = write_pi_creation_records(root, workspace, records)
    FileUtils.mkdir_p(authority, mode: 0700)
    authority_path = File.join(authority, slug + '.json')
    File.write(authority_path, JSON.generate('schema' => 2, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => workspace, 'slug' => slug,
      'session_incarnation' => 'a' * 64, 'binding_revision' => 2, 'generation' => 'e' * 64, 'state' => 'ready'), mode: 'w', perm: 0600)
    [runner, records, paths, authority_path]
  end

  def pi_creation_records(workspace, slug: 'example', ready: true)
    conversations = [{'address' => 'lead', 'incarnation' => 'b' * 64}, {'address' => 'implementer0', 'incarnation' => 'c' * 64}]
    policy = {'session' => {'workspace' => workspace, 'slug' => slug, 'incarnation' => 'a' * 64}, 'revision' => 1,
      'default' => {'revision' => 1, 'selection' => {'chain' => [{'provider_id' => 'host', 'account_ref' => 'account', 'model_id' => 'model', 'thinking' => 'high'}],
        'budget' => {'max_extra_attempts' => 0, 'max_hops' => 0}, 'fallback_on' => []}}, 'overrides' => []}
    selection = {'schema' => 1, 'catalog_digest' => '1' * 64, 'policy' => policy,
      'members' => conversations.map { |member| member.merge('source' => 'session_default', 'policy_digest' => 'f' * 64,
        'settings' => {'provider' => 'native', 'account_ref' => 'account', 'model' => 'model', 'thinking' => 'high', 'policy_revision' => 'f' * 64, 'revision' => 1}) }}
    team = {'id' => 'delegated', 'name' => 'Fixture', 'description' => 'Fixture team', 'roles' => %w[lead implementer0], 'catalogDigest' => '2' * 64, 'teamDigest' => '3' * 64,
      'leadModel' => 'model', 'leadEffort' => 'high', 'members' => [{'role' => 'implementer', 'address' => 'implementer0', 'model' => 'model', 'reasoningEffort' => 'high', 'behavior' => 'implementer', 'access' => 'workspace_write'}]}
    goal = 'The original real request'
    intent = {'schema' => 4, 'workspace' => workspace, 'slug' => slug, 'backend' => 'pi', 'backend_contract' => 1, 'receipt_id' => 'd' * 64,
      'deletion_history_sha256' => Digest::SHA256.hexdigest('[]'), 'session_incarnation' => 'a' * 64, 'conversations' => conversations, 'goal_sha256' => Digest::SHA256.hexdigest(goal),
      'model' => 'model', 'effort' => 'high', 'provider' => 'native', 'provider_selection' => selection, 'direct_team' => team, 'initial_input_id' => '11111111-1111-4111-8111-111111111111', 'sensitive' => false}
    state = ready ? 'ready' : 'creating'
    binding = {'schema' => 1, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => workspace, 'slug' => slug, 'session_incarnation' => 'a' * 64,
      'creation_receipt_id' => 'd' * 64, 'revision' => ready ? 2 : 1, 'state' => state,
      'conversations' => conversations.map.with_index { |member, index| member.merge('state' => ready && index.zero? ? 'ready' : 'creating', **(ready && index.zero? ? {'native_id' => 'native-lead'} : {})) }}
    journal = intent.merge('state' => state, 'native_create_started' => ready)
    journal.merge!('native_generation' => 'e' * 64, 'native' => {'native_id' => 'native-lead', 'history_ref' => '9' * 64}) if ready
    now = Time.now.utc.iso8601
    receipt = {'schema' => 4, 'workspace' => workspace, 'request' => {'kind' => 'new', 'backend' => 'pi', 'slug' => slug, 'goal' => goal, 'team' => team['id'], 'catalogDigest' => team['catalogDigest']},
      'receiptId' => intent['receipt_id'], 'deletionHistorySha256' => intent['deletion_history_sha256'], 'attempt' => 1, 'state' => ready ? 'ready' : 'running', 'phase' => 'Fixture phase',
      'startedAt' => now, 'updatedAt' => now, 'validated' => true, 'goal' => goal, 'model' => 'model', 'effort' => 'high', 'directTeam' => team,
      'pi' => {'backendContract' => 1, 'sessionIncarnation' => 'a' * 64, 'conversations' => conversations, 'provider' => 'native', 'initialInputId' => intent['initial_input_id'], 'selection' => selection}}
    completion = {'schema' => 4, 'workspace' => workspace, 'slug' => slug, 'backend' => 'pi', 'backendContract' => 1, 'receiptId' => intent['receipt_id'], 'sessionIncarnation' => 'a' * 64,
      'bindingRevision' => 2, 'conversations' => conversations, 'nativeId' => 'native-lead', 'initialInputId' => intent['initial_input_id'], 'initialSubmissionDigest' => '4' * 64,
      'goalSha256' => intent['goal_sha256'], 'model' => 'model', 'effort' => 'high', 'provider' => 'native', 'providerSelection' => selection,
      'catalogDigest' => team['catalogDigest'], 'teamDigest' => team['teamDigest'], 'team' => team['id'], 'trackingDevice' => 1, 'trackingInode' => 2}
    {intent:, binding:, journal:, receipt:, completion:, goal:}
  end

  def write_pi_creation_records(root, workspace, records, receipt: true)
    slug = records.fetch(:intent).fetch('slug')
    receipt_path, journal_path = WorkspaceSessionBackends.creation_paths(root, workspace, slug)
    evidence = File.join(File.dirname(receipt_path), "#{slug}.#{records[:intent]['receipt_id']}.complete.json")
    paths = {binding: File.join(WorkspaceSessionBackends.binding_directory(root, workspace), "#{slug}.json"), journal: journal_path,
      intent: evidence + '.request', completion: evidence}
    paths[:receipt] = receipt_path if receipt
    paths.each do |key, path|
      FileUtils.mkdir_p(File.dirname(path), mode: 0700)
      File.write(path, JSON.generate(records.fetch(key)), mode: 'w', perm: 0600)
    end
    paths
  end

  def pi_fork_join_records(workspace, slug: 'example')
    records = pi_creation_records(workspace, slug:)
    intent = records.fetch(:intent)
    intent.delete('goal_sha256'); intent.delete('initial_input_id'); intent['schema'] = 5
    source = {'Workspace' => workspace, 'Slug' => 'source', 'Incarnation' => '6' * 64}
    ids = %w[aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb]
    members = intent['conversations'].map.with_index do |reserved, index|
      source_incarnation = (index.zero? ? '1' : '2') * 64
      source_native = index.zero? ? 'source-lead' : 'source-member'
      settings = intent['provider_selection']['members'][index]['settings']
      {'address' => reserved['address'], 'source_incarnation' => source_incarnation, 'creation_id' => index.zero? ? intent['receipt_id'] : '8' * 64,
        'state' => 'ready', 'added_at' => Time.now.utc.iso8601, 'source_policy_stored' => false, 'native_id' => ids[index],
        'source_selection' => intent['provider_selection']['policy']['default'], 'selection_source' => 'session_default',
        'source_catalog_digest' => intent['provider_selection']['catalog_digest'], 'source_policy_revision' => 1,
        'origin' => {'Source' => {'Conversation' => {'Session' => source, 'Backend' => 'pi', 'Member' => reserved['address'], 'Incarnation' => source_incarnation, 'NativeID' => source_native}, 'Generation' => 'e' * 64},
          'Snapshot' => {'Native' => {'sourceFile' => File.join(workspace, "#{reserved['address']}.jsonl"), 'sourceCwd' => File.join(workspace, 'work/source'), 'sourceNativeId' => source_native, 'entriesDigest' => '3' * 64, 'entryCount' => 2, 'leafId' => 'leaf'},
            'HistoryRef' => '4' * 64, 'Settings' => {'Provider' => settings['provider'], 'AccountRef' => settings['account_ref'], 'Model' => settings['model'], 'Thinking' => settings['thinking'], 'PolicyRevision' => settings['policy_revision'], 'Revision' => settings['revision']},
            'Instructions' => '', 'ToolPolicy' => nil}}}
    end
    fork = {'source' => source, 'source_creation_id' => '5' * 64, 'source_binding_revision' => 2, 'source_roster_revision' => 1,
      'selected_member' => 'implementer0', 'lead_access' => 'workspace_write', 'runtime_request_id' => '33333333-3333-4333-8333-333333333333', 'members' => members}
    intent['fork'] = fork
    records[:binding]['conversations'].each_with_index { |member, index| member.merge!('state' => 'ready', 'native_id' => ids[index]) }
    records[:journal] = intent.merge('state' => 'ready', 'native_create_started' => true, 'native_generation' => 'e' * 64, 'native' => {'native_id' => ids[0], 'history_ref' => '9' * 64})
    receipt = records.fetch(:receipt)
    receipt.delete('goal'); receipt['schema'] = 5
    receipt['request'] = {'kind' => 'fork', 'slug' => slug, 'backend' => 'pi', 'source' => 'source', 'selectedMember' => 'implementer0'}
    receipt['pi'].delete('initialInputId'); receipt['pi']['fork'] = fork
    completion = records.fetch(:completion)
    %w[initialInputId initialSubmissionDigest goalSha256].each { |key| completion.delete(key) }
    completion.merge!('schema' => 5, 'nativeId' => ids[0], 'fork' => fork)
    records
  end


  # Matches session.PiLifecycleCapture / piruntime.LifecycleMember on the wire.
  def pi_lifecycle_capture_record(records, authority, runtime, selectors: [])
    fork = records[:intent]['schema'] == 5
    members = records[:binding]['conversations'].map.with_index do |bound, i|
      selection = records[:intent]['provider_selection']['members'].fetch(i)['settings']
      settings = selection.to_h { |k, v| [k.split('_').map(&:capitalize).join, v] }
      settings['PolicyRevision'] = selection['policy_revision']
      settings['AccountRef'] = selection['account_ref']
      slot = {'Member' => bound['address'], 'Incarnation' => bound['incarnation'], 'State' => bound['state'],
        'CreationID' => i.zero? ? records[:intent]['receipt_id'] : (fork ? records[:intent]['fork']['members'][i]['creation_id'] : records[:intent]['receipt_id']),
        'InitialInputID' => i.zero? && !fork ? records[:intent]['initial_input_id'] : '', 'NativeID' => bound.fetch('native_id', ''), 'InitialSettings' => settings}
      member = {'Slot' => slot}
      if bound['native_id']
        member.merge!('Conversation' => {'Session' => runtime['Session'], 'Backend' => 'pi', 'Member' => bound['address'],
          'Incarnation' => bound['incarnation'], 'NativeID' => bound['native_id']}, 'Generation' => runtime['Generation'], 'Settings' => settings, 'HistoryRef' => '9' * 64)
      end
      member
    end
    {'kind' => 'pi', 'contract' => 1, 'binding' => records[:binding], 'authority' => authority,
      'runtime' => runtime, 'roster_revision' => fork ? 1 : 0, 'members' => members, 'selectors' => selectors}
  end

  # Ruby orchestration fixture, not native qualification. The disposable process
  # supplies typed Pi CLI results and verifies the real admitted parent's EX FD.
  def pi_lifecycle_fixture(workspace, fork: true)
    slug = '2026-06-06-pi-lifecycle'
    profile = File.join(workspace, '.fixture-profile')
    contract = File.join(profile, 'share/workspace-portal/runtime-contract.json')
    FileUtils.mkdir_p(File.dirname(contract))
    File.write(contract, JSON.generate('piLifecycle' => 1))
    script = File.join(workspace, '.fixture-pi-lifecycle.rb')
    env = {'DEV_WORKSPACE_DEFER_TRACKING' => '1', 'DEV_WORKSPACES_PROFILE' => profile,
      DevSession::ENV_CODEX_SOCKET => File.join(workspace, '.fixture-codex.sock'),
      DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, script].shelljoin}
    runner, records, paths, authority_path = pi_ready_session_fixture(workspace, fork:, env:, slug:)
    state_root = runner.instance_variable_get(:@workspace_state_root)
    if fork
      members = records[:intent]['fork']['members'].drop(1).map do |m|
        {'address' => m['address'], 'incarnation' => records[:binding]['conversations'].find { |b| b['address'] == m['address'] }['incarnation'],
          'creationId' => m['creation_id'], 'fork' => {'rootCreationId' => records[:intent]['receipt_id'], 'nativeId' => m['native_id']}}
      end
      roster = {'schema' => 4, 'revision' => 1, 'forkCreationId' => records[:intent]['receipt_id'], 'workspace' => workspace, 'slug' => slug,
        'sessionIncarnation' => 'a' * 64, 'leadIncarnation' => 'b' * 64, 'members' => members}
      id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      paths[:team] = File.join(state_root, 'codex-teams', id, slug + '.json')
      FileUtils.mkdir_p(File.dirname(paths[:team]), mode: 0700)
      File.write(paths[:team], JSON.generate(roster), mode: 'w', perm: 0600)
    end
    runtime_dir = File.join(state_root, 'pi-runtime', Digest::SHA256.hexdigest(workspace), slug, 'a' * 64)
    runtime = {'Session' => {'Workspace' => workspace, 'Slug' => slug, 'Incarnation' => 'a' * 64},
      'InstanceID' => '11111111-1111-4111-8111-111111111111', 'RequestID' => '22222222-2222-4222-8222-222222222222',
      'Digest' => 'd' * 64, 'Generation' => 'e' * 64}
    FileUtils.mkdir_p(runtime_dir, mode: 0700)
    runtime_path = File.join(runtime_dir, 'runtime.json')
    File.write(runtime_path, JSON.generate('Schema' => 2, 'CurrentRequestID' => runtime['RequestID'], 'Requests' => {
      '00000000-0000-4000-8000-000000000000' => {'Runtime' => runtime.merge('RequestID' => '00000000-0000-4000-8000-000000000000'), 'AuthDirectory' => '/must-not-use-obsolete', 'HostStateRoot' => '/must-not-use-obsolete'},
      runtime['RequestID'] => {'Runtime' => runtime, 'Phase' => 'starting', 'AuthDirectory' => File.join(workspace, '.fixture-auth'), 'HostStateRoot' => File.join(workspace, '.fixture-host')}}), mode: 'w', perm: 0600)
    audit = {}
    capture = pi_lifecycle_capture_record(records, JSON.parse(File.read(authority_path)), runtime)
    capture['members'].each do |member|
      next unless member['Conversation']
      bound = records[:binding]['conversations'].find { |b| b['address'] == member['Slot']['Member'] }
      path = File.join(runtime_dir, 'members', bound['address'], bound['incarnation'], 'conversation.json')
      FileUtils.mkdir_p(File.dirname(path), mode: 0700)
      File.write(path, JSON.generate(member), mode: 'w', perm: 0600)
      audit[path] = File.binread(path)
    end
    selector_paths = {'binding' => paths[:binding], 'creation' => paths[:journal], 'receipt' => paths[:receipt],
      'completion' => paths[:completion], 'companion' => paths[:intent], 'team' => paths[:team], 'authority' => authority_path}.compact
    capture['selectors'] = selector_paths.map { |name, path| {'Name' => name, 'Path' => path, 'SHA256' => Digest::SHA256.file(path).hexdigest} }
    fixture_dir = File.join(state_root, 'fixture-lifecycle')
    FileUtils.mkdir_p(fixture_dir, mode: 0700)
    File.write(File.join(fixture_dir, 'capture.json'), JSON.generate(capture), mode: 'w', perm: 0600)
    File.write(script, <<~'RUBY')
      require 'json'
      require 'digest'
      # No upload state exists in this fixture; its separate owner has no work.
      exit 0 if ARGV[0, 2] == ['uploads', 'remove-session']
      abort 'unexpected native operation' unless ARGV.shift(2) == ['pi-runtime', 'lifecycle']
      args = ARGV.each_slice(2).to_h
      root = File.join(args.fetch('--user-state-root'), 'fixture-lifecycle')
      action = args.fetch('--action')
      allowed = %w[capture preflight capture-force-delete check archive-retire delete-retire retire terminal-delete finalize-authority prepare-restore restore]
      abort 'Create/Send or unknown lifecycle operation' unless allowed.include?(action)
      capture = STDIN.tty? ? nil : STDIN.read
      capture = capture.nil? || capture.empty? ? JSON.parse(File.read(File.join(root, 'capture.json'))) : JSON.parse(capture)
      inherited = false
      unless action == 'preflight'
        fd = IO.for_fd(Integer(ENV.fetch('DEV_SESSION_LIFECYCLE_LOCK_FD')), autoclose: false)
        lock_path = ENV.fetch('DEV_SESSION_LIFECYCLE_LOCK_PATH')
        abort 'changed inherited lock' unless fd.stat.ino == File.stat(lock_path).ino && fd.stat.dev == File.stat(lock_path).dev
        File.open(lock_path, 'r+') { |independent| abort 'parent EX not held' if independent.flock(File::LOCK_EX | File::LOCK_NB) }
        abort 'no admitted lifecycle operation' unless %w[archive delete revive].include?(ENV['DEV_SESSION_LIFECYCLE_OPERATION'])
        inherited = true
      end
      workspace, slug = args.values_at('--workspace', '--slug')
      if ENV['DEV_SESSION_LIFECYCLE_OPERATION'] == 'delete' && %w[capture capture-force-delete].include?(action)
        # The real Go creation-provenance reader requires every matching removal
        # directory to have its marker, even before it checks terminal state.
        id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
        Dir.glob(File.join(args.fetch('--user-state-root'), 'removed', id, "*-#{slug}-*")).each do |directory|
          abort 'open removal recovery marker: no such file or directory' unless File.file?(File.join(directory, 'recovery.json'))
        end
      end
      journal_path = File.join(workspace, 'worktrees/.locks', slug + '.' + (ENV['DEV_SESSION_LIFECYCLE_OPERATION'] == 'delete' ? 'removal' : ENV.fetch('DEV_SESSION_LIFECYCLE_OPERATION', 'archive')) + '.json')
      journal = File.file?(journal_path) ? JSON.parse(File.read(journal_path)) : nil
      File.open(File.join(root, 'calls.jsonl'), 'a') { |f| f.puts JSON.generate('action' => action, 'capture' => capture, 'inherited' => inherited, 'journal' => journal, 'args' => args) }
      abort 'obsolete runtime config used' unless args['--pi-auth-dir'] == File.join(workspace, '.fixture-auth') && args['--pi-host-state-root'] == File.join(workspace, '.fixture-host')
      fault = File.join(root, 'fail-' + action)
      if File.file?(fault)
        File.unlink(fault)
        abort 'injected interrupted lifecycle response'
      end
      case action
      when 'archive-retire', 'delete-retire', 'retire', 'terminal-delete'
        r = capture.fetch('runtime')
        capture['terminal'] = {'schema' => 1, 'host_launcher_contract' => 1,
          'scope' => {'workspace' => workspace, 'slug' => slug, 'session_incarnation' => capture['binding']['session_incarnation'], 'backend' => 'pi', 'backend_contract' => 1, 'binding_revision' => capture['binding']['revision']},
          'instance_id' => r['InstanceID'], 'request_id' => r['RequestID'], 'generation' => r['Generation'], 'request_sha256' => r['Digest'],
          'app_store_path' => '/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-fixture-app', 'profile_path' => args['--host-profile'], 'profile_generation' => '1' * 64,
          'phase' => 'terminal', 'outcome' => 'stopped', 'result' => 'stopped', 'unit' => 'workspace-pi-session@fixture.service', 'boot_id' => '11111111-1111-4111-8111-111111111111', 'invocation_id' => '1' * 32, 'control_group' => '/fixture',
          'load_state' => 'loaded', 'active_state' => 'inactive', 'job' => '',
          'drain_proof' => {'empty' => true, 'boot_id' => '11111111-1111-4111-8111-111111111111', 'invocation_id' => '1' * 32,
            'control_group' => '/fixture', 'load_state' => 'loaded', 'active_state' => 'inactive', 'job' => ''}}
      when 'finalize-authority'
        path = File.join(args.fetch('--authority-dir'), slug + '.json')
        abort 'changed live authority' unless JSON.parse(File.read(path)) == capture['authority']
        File.unlink(path)
      when 'prepare-restore'
        capture['successor'] = {'Session' => capture['runtime']['Session'], 'BindingRevision' => capture['binding']['revision'],
          'RequestID' => '33333333-3333-4333-8333-333333333333', 'ReplacesRequestID' => capture['runtime']['RequestID'],
          'AppStorePath' => '/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-fixture-app', 'ProfilePath' => args['--host-profile'], 'ProfileGeneration' => '1' * 64}
      when 'restore'
        abort 'missing frozen successor' unless capture['successor']
        path = File.join(args.fetch('--authority-dir'), slug + '.json')
        File.write(path, JSON.generate(capture['authority']), mode: 'w', perm: 0600) unless File.exist?(path)
        abort 'changed authority' unless JSON.parse(File.read(path)) == capture['authority']
      end
      puts JSON.generate(capture)
    RUBY
    commit_tracking(workspace, slug, lifecycle: 'active')
    configure_workspace_origin(workspace)
    runner.define_singleton_method(:start) { |*, **| raise 'Pi revive must not Create/Send/start a Codex session' }
    {runner:, records:, paths:, authority: authority_path, capture:, runtime_path:, audit:, fixture_dir:, profile:, slug:, state_root:}
  end

  def pi_lifecycle_calls(fixture)
    path = File.join(fixture.fetch(:fixture_dir), 'calls.jsonl')
    File.file?(path) ? File.readlines(path).map { |line| JSON.parse(line) } : []
  end

end
