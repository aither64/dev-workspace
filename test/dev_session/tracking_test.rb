# frozen_string_literal: true

require_relative '../support/dev_session_test_case'
require_relative '../support/pi_creation_fixture'

class DevSessionTest < Minitest::Test
  include PiCreationFixture

  def test_provider_binding_owning_manifest_authority_and_terminal_projections
    with_workspace do |workspace|
      slug,root,incarnation='bound','original-root','a'*64
      socket='/run/work/app-server.sock'
      selected=DevSession::Tmux::Session.new(id:'$1',name:slug,mark:'1',slug:slug,workspace:workspace,environment_slug:slug,
        socket_path:File.join(workspace,'tmux.sock'),codex_thread_id:root,codex_socket_path:socket,codex_client_version:'0.172.0',codex_pane_id:'%1',identity_token:incarnation)
      File.write(selected.socket_path, '') # Socket identity is doubled in this owning projection control.
      calls=[];start_command='';current_command='codex';terminal_snapshot=nil;recorded_before_pause=[]
      tmux=Object.new
      tmux.define_singleton_method(:session_by_id) { |id| id=='$1' ? selected : nil }
      tmux.define_singleton_method(:pane_session_id) { |_pane| '$1' }
      tmux.define_singleton_method(:pane_current_command) { |_pane| current_command }
      status=Struct.new(:exitstatus).new(0)
      tmux.define_singleton_method(:capture) { |*_args| [JSON.generate(start_command)+"\n",'',status] }
      tmux.define_singleton_method(:run) do |*args|
        calls << args
        selected.codex_thread_id=args.last if args.first=='set-option'
        if args.first=='respawn-pane'
          recorded_before_pause << JSON.parse(File.read(terminal_snapshot)).fetch('terminals').length if terminal_snapshot
          start_command=args.last
          current_command=start_command.include?(' resume ') ? 'codex' : File.basename(start_command)
        end
      end
      state_root=File.join(workspace,'.provider-state')
      runner=runner_for(workspace,tmux:tmux,authority_dir:File.join(workspace,'.authority'),env:{'DEV_WORKSPACES_STATE'=>state_root,'DEV_WORKSPACE_NAME'=>'work'})
      runner.ensure_tracking_files(slug)
      original={'schema'=>1,'slug'=>slug,'repositories'=>[],'codex'=>{'thread_id'=>root,'socket_path'=>socket,'client_version'=>'0.172.0'}}
      runner.send(:write_portal_manifest,slug,original)
      authority={'schema'=>1,'state'=>'ready','slug'=>slug,'workspace'=>workspace,'tmux_socket'=>selected.socket_path,'tmux_session_id'=>'$1','tmux_identity'=>incarnation,
        'codex_thread_id'=>root,'codex_socket_path'=>socket,'codex_client_version'=>'0.172.0'}
      runner.send(:write_session_authority_record!,slug,authority)
      runner.send(:project_provider_manifest!,slug,root,1,socket)
      manifest=runner.send(:load_portal_manifest,runner.send(:portal_file,slug),required:true)
      assert_equal [6,3,1,root],[manifest['schema'],manifest.dig('codex','binding_schema'),manifest.dig('codex','binding_origin_schema'),manifest.dig('codex','thread_id')]
      before=File.binread(runner.send(:portal_file,slug))
      runner.send(:project_provider_manifest!,slug,root,1,socket)
      assert_equal before,File.binread(runner.send(:portal_file,slug))
      assert_raises(DevSession::Error) { runner.send(:project_provider_manifest!,slug,root,3,socket) }
      source_binding={'rootThreadId'=>root,'incarnation'=>incarnation,'revision'=>1,'conversations'=>{root=>{'threadId'=>root,'member'=>'lead','provider'=>'openai'}}}
      runner.send(:project_provider_authority!,slug,source_binding,socket)
      directory=File.join(state_root,'providers','work');FileUtils.mkdir_p(directory,mode:0700)
      operation='aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      File.write(File.join(directory,'state.json'),JSON.generate('schema'=>3,'workspace'=>'work','root'=>workspace,'socket'=>socket,'operation'=>{'id'=>operation,'phase'=>'applying'}),mode:'w',perm:0600)
      terminal_snapshot=File.join(directory,operation+'.snapshot.json.terminals')
      File.write(terminal_snapshot,JSON.generate('schema'=>1,'workspace'=>'work','root'=>workspace,'socket'=>socket,'terminals'=>[]),mode:'w',perm:0600)
      runner.send(:project_provider_source_pause!,slug,source_binding,root,terminal_snapshot)
      assert_equal([1],recorded_before_pause,'terminal record must precede the owned pause')
      assert_equal(File.basename(runner.instance_variable_get(:@shell_command)),current_command)
      saved_terminal=File.binread(terminal_snapshot);paused_calls=calls.length
      runner.send(:project_provider_source_pause!,slug,source_binding,root,terminal_snapshot)
      assert_equal(paused_calls,calls.length,'pause retry restarted the already paused pane')
      assert_equal(saved_terminal,File.binread(terminal_snapshot))
      assert_raises(DevSession::Error) { runner.send(:project_provider_source_pause!,slug,source_binding,'foreign-source',terminal_snapshot) }
      assert_equal(paused_calls,calls.length)
      binding={'rootThreadId'=>root,'incarnation'=>incarnation,'revision'=>2,'conversations'=>{root=>{'threadId'=>'current-child','member'=>'lead','provider'=>'openai'}}}
      runner.send(:project_provider_authority!,slug,binding,socket)
      active=runner.send(:load_session_authority,slug,required:true)
      assert_equal [3,root,'current-child',2,incarnation],active.values_at('schema','codex_thread_id','codex_active_thread_id','codex_binding_revision','tmux_identity')
      runner.send(:project_provider_terminal!,slug,binding,root,[],'retained-model','high')
      assert_equal 'current-child',selected.codex_thread_id
      assert_includes start_command,'resume current-child'
      assert_includes start_command,'retained-model'
      assert_equal 3,calls.count { |args| args.first=='respawn-pane' }
      count=calls.length
      runner.send(:project_provider_terminal!,slug,binding,root,[],'retained-model','high')
      assert_equal count,calls.length,'effect-before-flag recovery restarted exact child terminal'
      current_command='editor'
      assert_raises(DevSession::Error) { runner.send(:project_provider_terminal!,slug,binding,root,[],'retained-model','high') }
      assert_equal count,calls.length
      assert_equal root,runner.send(:load_session_authority,slug,required:true)['codex_thread_id']
    end
  end

  def test_provider_terminal_projection_with_real_tmux
    skip 'real tmux tests disabled' if ENV['DEV_SESSION_SKIP_REAL_TMUX_TESTS'] == '1'

    with_workspace do |workspace|
      slug, root, child, incarnation = 'bound', 'original-root', 'current-child', 'a' * 64
      socket = File.join(workspace, 'tmux.sock')
      native_socket = File.join(workspace, 'app-server.sock')
      command_runner = DevSession::CommandRunner.new(out: StringIO.new, err: StringIO.new)
      tmux = DevSession::Tmux.new(runner: command_runner, socket:)
      shell = ENV.fetch('SHELL')
      cli = File.join(workspace, 'inert cli.rb')
      started = File.join(workspace, 'cli-started')
      File.write(cli, "File.write(#{started.inspect}, Process.pid.to_s)\nsleep 60\n")
      state_root = File.join(workspace, '.provider-state')
      runner = runner_for(workspace, tmux:, authority_dir: File.join(workspace, '.authority'), env: {
        'DEV_WORKSPACES_STATE' => state_root, 'DEV_WORKSPACE_NAME' => 'work', 'SHELL' => shell,
        DevSession::ENV_CODEX_COMMAND => [RbConfig.ruby, cli].shelljoin
      })
      runner.ensure_tracking_files(slug)
      tmux.run('-f', File::NULL, 'new-session', '-d', '-s', slug, '-c', workspace, shell)
      selected = tmux.session(slug)
      target = DevSession::Tmux.session_target(selected.id)
      pane = tmux.capture('display-message', '-p', '-t', target, '#{pane_id}').first.strip
      {DevSession::SESSION_MARK => '1', DevSession::SESSION_SLUG => slug,
        DevSession::SESSION_CODEX_THREAD => root, DevSession::SESSION_CODEX_SOCKET => native_socket,
        DevSession::SESSION_CODEX_VERSION => '0.172.0', DevSession::SESSION_CODEX_PANE => pane}.each do |key, value|
        tmux.run('set-option', '-t', target, key, value)
      end
      {DevSession::ENV_SLUG => slug, DevSession::ENV_WORKSPACE => workspace,
        DevSession::ENV_TMUX_IDENTITY => incarnation}.each do |key, value|
        tmux.run('set-environment', '-t', target, key, value)
      end
      runner.send(:write_portal_manifest, slug, {'schema' => 1, 'slug' => slug, 'repositories' => [],
        'codex' => {'thread_id' => root, 'socket_path' => native_socket, 'client_version' => '0.172.0'}})
      runner.send(:write_session_authority_record!, slug, {'schema' => 1, 'state' => 'ready', 'slug' => slug,
        'workspace' => workspace, 'tmux_socket' => socket, 'tmux_session_id' => selected.id, 'tmux_identity' => incarnation,
        'codex_thread_id' => root, 'codex_socket_path' => native_socket, 'codex_client_version' => '0.172.0'})
      runner.send(:project_provider_manifest!, slug, root, 1, native_socket)
      source_binding = {'rootThreadId' => root, 'incarnation' => incarnation, 'revision' => 1,
        'conversations' => {root => {'threadId' => root, 'member' => 'lead', 'provider' => 'openai'}}}
      runner.send(:project_provider_authority!, slug, source_binding, native_socket)
      directory = File.join(state_root, 'providers', 'work')
      FileUtils.mkdir_p(directory, mode: 0700)
      operation = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      File.write(File.join(directory, 'state.json'), JSON.generate('schema' => 3, 'workspace' => 'work',
        'root' => workspace, 'socket' => native_socket, 'operation' => {'id' => operation, 'phase' => 'applying'}), perm: 0600)
      snapshot = File.join(directory, operation + '.snapshot.json.terminals')
      File.write(snapshot, JSON.generate('schema' => 1, 'workspace' => 'work', 'root' => workspace,
        'socket' => native_socket, 'terminals' => []), perm: 0600)
      runner.send(:project_provider_source_pause!, slug, source_binding, root, snapshot)
      binding = {'rootThreadId' => root, 'incarnation' => incarnation, 'revision' => 2,
        'conversations' => {root => {'threadId' => child, 'member' => 'lead', 'provider' => 'openai'}}}
      runner.send(:project_provider_authority!, slug, binding, native_socket)
      arguments = ['-c', 'model_provider="public-provider"']
      expected = runner.send(:native_client_arguments, slug, model: 'retained-model', effort: 'high',
        provider_arguments: arguments).map(&:shellescape).join(' ')
      expected += " --remote #{("unix://" + native_socket).shellescape} resume #{child.shellescape}"
      runner.send(:project_provider_terminal!, slug, binding, root, arguments, 'retained-model', 'high')
      actual = tmux.session_by_id(selected.id)
      assert actual, 'final session exists'
      assert_equal child, actual.codex_thread_id, 'final child binding'
      assert_equal incarnation, actual.identity_token, 'final immutable identity'
      readback = tmux.capture('display-message', '-p', '-t', actual.codex_pane_id, '#{pane_start_command}')
      assert readback.last.success?
      assert_equal (JSON.generate(expected) + "\n").b, readback.first.b, 'exact serialized pane_start_command'
      assert_equal [selected.id, pane, slug, workspace], [actual.id, actual.codex_pane_id, actual.slug, actual.workspace]
      wait_for_file(started)
      pane_process = tmux.capture('display-message', '-p', '-t', pane, '#{pane_pid}').first.strip
      assert_equal File.read(started), pane_process
      assert_equal File.basename(RbConfig.ruby), tmux.pane_current_command(pane)
      assert_equal actual, runner.send(:project_provider_terminal!, slug, binding, root, arguments, 'retained-model', 'high')
      assert_equal pane_process, tmux.capture('display-message', '-p', '-t', pane, '#{pane_pid}').first.strip,
        'idempotent projection must not respawn the owned pane'
      assert_equal (JSON.generate(expected) + "\n").b, tmux.capture('display-message', '-p', '-t', pane, '#{pane_start_command}').first.b
      tmux.run('set-environment', '-t', target, DevSession::ENV_TMUX_IDENTITY, 'b' * 64)
      error = assert_raises(DevSession::Error) do
        runner.send(:project_provider_terminal!, slug, binding, root, arguments, 'retained-model', 'high')
      end
      assert_equal 'terminal projection owner changed', error.message
      assert_equal pane_process, tmux.capture('display-message', '-p', '-t', pane, '#{pane_pid}').first.strip
    ensure
      tmux&.capture('kill-server', allow_failure: true)
    end
  end

  def test_provider_terminal_command_readback_is_one_exact_string
    with_workspace do |workspace|
      command = "ruby public\\ path 'single' \"double\" $HOME ; $(false) \\\\n\tline\n --remote unix:///public resume child "
      stdout = JSON.generate(command) + "\n"
      status = Struct.new(:exitstatus).new(0)
      failure = nil
      tmux = Object.new
      tmux.define_singleton_method(:capture) do |*_args|
        raise failure if failure
        [stdout, 'public diagnostic outside stdout', status]
      end
      runner = runner_for(workspace, tmux:)
      assert runner.send(:provider_terminal_command_matches?, '%1', command)
      [JSON.generate(command), JSON.generate(command) + "\n\n", "\n#{JSON.generate(command)}\n"].each do |framing|
        stdout = framing
        refute runner.send(:provider_terminal_command_matches?, '%1', command), 'exactly one framing LF required'
      end
      [JSON.generate(command.strip), JSON.generate(command.gsub("\n", '\\n')),
        JSON.generate('different command'), 'null', '["command"]', '"unterminated',
        JSON.generate(command) + ' "extra"', %q{"command \141"}, %q{"command \v"}].each do |invalid|
        stdout = invalid + "\n"
        refute runner.send(:provider_terminal_command_matches?, '%1', command), invalid.inspect
      end
      {%q{"command \v"} => 'command v', %q{"command \141"} => 'command 141'}.each do |invalid, expected|
        stdout = invalid + "\n"
        refute runner.send(:provider_terminal_command_matches?, '%1', expected), 'unsupported escape must refuse'
      end
      failure = DevSession::CommandError.new(['tmux', 'display-message'], Struct.new(:exitstatus).new(1),
        JSON.generate(command), 'public command failure')
      assert_same failure, assert_raises(DevSession::CommandError) {
        runner.send(:provider_terminal_command_matches?, '%1', command)
      }
    end
  end

  def test_bound_terminal_recovery_uses_sealed_current_route_and_immutable_incarnation
    with_workspace do |workspace|
      slug, root, child, incarnation = 'bound', 'original-root', 'current-child', 'a' * 64
      socket = '/run/work/app-server.sock'
      state_root = File.join(workspace, '.binding-state')
      directory = File.join(state_root, 'providers', 'work')
      FileUtils.mkdir_p(directory, mode: 0700)
      catalog = File.join(workspace, 'catalog.json')
      File.write(catalog, JSON.generate('schema' => 1, 'initialProvider' => 'openai',
        'providers' => [{'id' => 'openai', 'label' => 'Subscription', 'kind' => 'subscription'}]))
      operation = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
      key = {'workspace' => workspace, 'slug' => slug, 'rootThreadId' => root,
        'incarnation' => incarnation, 'member' => 'lead', 'originalThreadId' => root}
      evidence = JSON.generate('schema' => 1, 'operationId' => operation, 'workspace' => 'work',
        'root' => workspace, 'socket' => socket, 'previousBindingRevision' => 1,
        'committedBindingRevision' => 2, 'snapshotDigest' => 'c' * 64,
        'participants' => [{'key' => key, 'oldThreadId' => root, 'newThreadId' => child,
          'oldProvider' => 'openai', 'newProvider' => 'openai'}])
      proof_path = File.join(directory, "#{operation}.snapshot.json.evidence")
      File.write(proof_path, evidence, mode: 'w', perm: 0600)
      binding = {'rootThreadId' => root, 'incarnation' => incarnation, 'revision' => 2,
        'conversations' => {root => {'member' => 'lead', 'threadId' => child, 'provider' => 'openai',
          'operationId' => operation, 'evidenceDigest' => Digest::SHA256.hexdigest(evidence)}}}
      state = {'schema' => 3, 'workspace' => 'work', 'root' => workspace, 'socket' => socket,
        'active' => 'openai', 'bindingRevision' => 2, 'bindings' => {slug => binding}}
      state_path = File.join(directory, 'state.json')
      File.write(state_path, JSON.generate(state), mode: 'w', perm: 0600)
      runner = runner_for(workspace, authority_dir: File.join(workspace, '.authority'), env: {
        'DEV_WORKSPACES_STATE' => state_root, 'DEV_WORKSPACE_NAME' => 'work',
        'DEV_WORKSPACES_CODEX_PROVIDERS' => catalog, DevSession::ENV_CODEX_SOCKET => socket,
        DevSession::ENV_CODEX_VERSION => '0.172.0'})
      runner.ensure_tracking_files(slug)
      manifest = {'schema' => 6, 'slug' => slug, 'repositories' => [], 'artifacts' => [],
        'codex' => {'thread_id' => root, 'socket_path' => socket, 'client_version' => '0.172.0',
          'binding_schema' => 3, 'binding_origin_schema' => 1}}
      runner.send(:write_portal_manifest, slug, manifest)
      assert_equal [child, incarnation], runner.send(:bound_provider_route!, slug, manifest)
      refute_path_exists(runner.send(:authority_file, slug))
      journal = runner.send(:prepare_start_journal!, slug, preferred_identity: incarnation)
      path = runner.send(:start_journal_file, slug)
      saved = File.binread(path)
      assert_equal journal, runner.send(:prepare_start_journal!, slug, preferred_identity: incarnation)
      assert_equal saved, File.binread(path)
      assert_raises(DevSession::Error) { runner.send(:prepare_start_journal!, slug, preferred_identity: 'f' * 64) }
      assert_equal saved, File.binread(path)
      selected = DevSession::Tmux::Session.new(id: '$1', name: slug, mark: '1', slug: slug,
        workspace: workspace, environment_slug: slug, socket_path: '/run/work/tmux.sock',
        codex_thread_id: child, codex_socket_path: socket, codex_client_version: '0.172.0',
        codex_pane_id: '%1', identity_token: incarnation)
      runner.send(:write_session_authority, slug, selected, state: 'ready')
      authority = runner.send(:load_session_authority, slug, required: true)
      assert_equal [3, root, child, 2, incarnation], authority.values_at('schema', 'codex_thread_id',
        'codex_active_thread_id', 'codex_binding_revision', 'tmux_identity')
      assert_equal manifest, runner.send(:load_portal_manifest, runner.send(:portal_file, slug), required: true)
      selected.codex_thread_id = root
      before = File.binread(runner.send(:authority_file, slug))
      assert_raises(DevSession::Error) { runner.send(:write_session_authority, slug, selected, state: 'ready') }
      assert_equal before, File.binread(runner.send(:authority_file, slug))
      File.write(proof_path, evidence.sub(child, 'foreign-child'))
      assert_raises(DevWorkspaceHost::ProviderBindings::Error, DevSession::Error) { runner.send(:bound_provider_route!, slug, manifest) }
      assert_equal saved, File.binread(path)
    end
  end

  def test_bound_manifest_archive_revive_preserves_origin_and_creation_contract
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = 'bound'
      runner.ensure_tracking_files(slug)
      path = runner.send(:portal_file, slug)
      [1, 3].each do |origin|
        manifest = {'schema' => 6, 'slug' => slug, 'repositories' => [],
          'codex' => {'thread_id' => 'original-root', 'socket_path' => '/run/work/app-server.sock',
            'client_version' => '0.172.0', 'binding_schema' => 3, 'binding_origin_schema' => origin},
          'creation' => {'state' => 'ready', 'initial_goal_sent' => true}}
        runner.send(:write_portal_manifest, slug, manifest)
        runner.instance_variable_set(:@explicit_archive_operation_id, 'a' * 64)
        archived = runner.send(:finalized_portal_manifest, slug, nil, proven_heads: {}, finalized_at: '2026-11-25T12:00:00Z')
        assert_equal([6, origin, 'original-root'], [archived['schema'], archived.dig('codex', 'binding_origin_schema'), archived.dig('codex', 'thread_id')])
        assert_equal('a' * 64, archived['archive_operation_id'])
        runner.send(:apply_revived_portal_transition!, archived, slug, path, plan_sha256: 'b' * 64, state_sha256: 'c' * 64)
        assert_equal([6, origin, 'original-root'], [archived['schema'], archived.dig('codex', 'binding_origin_schema'), archived.dig('codex', 'thread_id')])
        refute(archived.key?('archive_operation_id'))
        refute(archived.key?('finalized_at'))
        assert_equal('revived', archived.dig('creation', 'tracking_origin'))
        pending = Marshal.load(Marshal.dump(archived))
        pending['creation'] = {'state' => 'creating', 'initial_goal_sent' => false, 'initial_goal_attempted' => true}
        assert_raises(DevSession::Error) { runner.send(:validate_portal_manifest!, pending, slug, path) }
        pending['codex']['binding_origin_schema'] = 2
        runner.send(:validate_portal_manifest!, pending, slug, path)
        pending['creation'] = {'state' => 'ready', 'initial_goal_sent' => true}
        runner.send(:validate_portal_manifest!, pending, slug, path)
        assert_equal(2, pending.dig('codex', 'binding_origin_schema'))
        ['', 'bad'].each do |operation|
          assert_raises(DevSession::Error) { runner.send(:validate_portal_manifest!, archived.merge('archive_operation_id' => operation), slug, path) }
        end
        assert_raises(DevSession::Error) { runner.send(:validate_portal_manifest!, archived.merge('archive_operation_id' => 'a' * 64), slug, path) }
      end
    end
  end

  def test_list_reads_pi_authority_without_tmux_and_refuses_conflicting_evidence
    with_workspace do |workspace|
      root, authority = File.join(workspace, '.pi-state'), File.join(workspace, '.pi-authority')
      slug = '2026-06-06-pi'
      records = pi_creation_records(workspace, slug:)
      paths = write_pi_creation_records(root, workspace, records)
      FileUtils.mkdir_p(authority, mode: 0700)
      path = File.join(authority, slug + '.json')
      ready = {'schema' => 2, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => workspace, 'slug' => slug,
        'session_incarnation' => 'a' * 64, 'binding_revision' => 2, 'generation' => 'e' * 64, 'state' => 'ready'}
      File.write(path, JSON.generate(ready), mode: 'w', perm: 0600)
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-06-codex'))
      tmux = NullTmux.new
      original = tmux.method(:session)
      calls = []
      tmux.define_singleton_method(:session) do |name|
        raise 'Pi must never query same-name tmux' if name == slug
        calls << name
        original.call(name)
      end
      out = StringIO.new
      runner = runner_for(workspace, tmux:, authority_dir: authority, env: {'DEV_WORKSPACES_STATE' => root}, out:)
      before = paths.transform_values { |p| File.binread(p) }
      runner.list
      assert_equal(['2026-06-06-codex'], calls)
      assert_match(/#{slug}.*none/, out.string)
      assert_match(/2026-06-06-codex.*none/, out.string)
      File.unlink(path)
      runner.list(slug, as_is: true) # Inventory is supported without live authority.
      refute_path_exists(path)
      %w[workspace session_incarnation binding_revision backend schema malformed].each do |fault|
        changed = ready.dup
        case fault
        when 'workspace' then changed['workspace'] += '-other'
        when 'session_incarnation' then changed['session_incarnation'] = 'f' * 64
        when 'binding_revision' then changed['binding_revision'] += 1
        when 'backend' then changed['backend'] = 'codex'
        when 'schema' then changed['schema'] = 3
        end
        bytes = fault == 'malformed' ? '{' : JSON.generate(changed)
        File.write(path, bytes, mode: 'w', perm: 0600)
        assert_raises(DevSession::Error) { runner.list(slug, as_is: true) }
        assert_equal(bytes, File.binread(path))
      end
      File.write(path, JSON.generate(ready), mode: 'w', perm: 0600)
      File.unlink(paths.fetch(:binding))
      assert_raises(DevSession::Error) { runner.list(slug, as_is: true) }
      refute_path_exists(paths.fetch(:binding))
      before.each { |key, bytes| assert_equal(bytes, File.binread(paths.fetch(key))) unless key == :binding }
    end
  end

  def test_pi_manifest_schema4_preserves_explicit_scope_without_codex_metadata
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-11-12-pi'
      runner.ensure_tracking_files(slug)
      manifest = {'schema' => 4, 'slug' => slug,
        'backend' => {'kind' => 'pi', 'contract' => 1, 'session_incarnation' => 'a' * 64},
        'creation' => {'state' => 'creating', 'goal_sha256' => 'b' * 64, 'initial_goal_sent' => false},
        'repositories' => []}
      runner.send(:write_portal_manifest, slug, manifest)
      path = runner.send(:portal_file, slug)
      assert_equal(manifest, runner.send(:load_portal_manifest, path, required: true))
      refute_includes(File.read(path), 'codex:')
      [{'codex' => {}}, {'tmux' => {}}, {'schema' => 1},
       {'creation' => manifest.fetch('creation').merge('state' => 'ready')},
       {'backend' => manifest.fetch('backend').merge('contract' => 2)}].each do |change|
        assert_raises(DevSession::Error) do
          runner.send(:validate_portal_manifest!, manifest.merge(change), slug, path)
        end
      end
      missing_scope = manifest.dup
      missing_scope.delete('repositories')
      assert_raises(DevSession::Error) { runner.send(:validate_portal_manifest!, missing_scope, slug, path) }
    end
  end

  def test_pi_fork_manifest_schema5_has_no_input_or_goal
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-11-18-fork'
      runner.ensure_tracking_files(slug)
      manifest = {'schema' => 5, 'slug' => slug, 'forked_from' => '2026-11-18-source',
        'backend' => {'kind' => 'pi', 'contract' => 1, 'session_incarnation' => 'a' * 64},
        'creation' => {'state' => 'ready'}, 'repositories' => []}
      runner.send(:write_portal_manifest, slug, manifest)
      path = runner.send(:portal_file, slug)
      assert_equal(manifest, runner.send(:load_portal_manifest, path, required: true))
      %w[goal_sha256 initial_goal_sent initial_goal_attempted].each do |key|
        assert_raises(DevSession::Error) { runner.send(:validate_portal_manifest!, manifest.merge('creation' => manifest['creation'].merge(key => false)), slug, path) }
      end
      refute_includes(File.read(path), 'codex:')
    end
  end

  def test_session_backend_guard_preserves_pi_bytes_and_prevents_legacy_writer_effects
    with_workspace do |workspace|
      root = File.join(workspace, '.test-user-state')
      authority = File.join(workspace, '.test-authority')
      runner = runner_for(workspace, env: {'DEV_WORKSPACES_STATE' => root, DevSession::ENV_AUTHORITY_DIR => authority})
      slug = '2026-11-12-pi'
      directory = WorkspaceSessionBackends.binding_directory(root, workspace)
      FileUtils.mkdir_p(directory, mode: 0700)
      path = File.join(directory, "#{slug}.json")
      binding = {'schema' => 1, 'backend' => 'pi', 'backend_contract' => 1,
        'workspace' => workspace, 'slug' => slug, 'session_incarnation' => 'a' * 64,
        'creation_receipt_id' => 'receipt', 'revision' => 1, 'state' => 'ready',
        'conversations' => [{'address' => 'lead', 'incarnation' => 'b' * 64, 'state' => 'ready', 'native_id' => 'pi-session'}]}
      bytes = JSON.generate(binding)
      File.write(path, bytes, mode: 'w', perm: 0600)
      [-> { runner.send(:write_session_authority, slug, Object.new, state: 'ready') },
       -> { runner.send(:delete_session_authority, slug) },
       -> { runner.send(:with_creation_lock, slug) { flunk('creation reached') } },
       -> { runner.send(:with_slug_lock, slug) { flunk('legacy runtime reached') } },
       -> { runner.start(slug, as_is: true, new: false, attach: false, run_codex: false) }].each do |operation|
        error = assert_raises(DevSession::Error, &operation)
        assert_includes(error.message, 'backend conflict')
        assert_equal(bytes, File.binread(path))
        refute(File.exist?(authority))
        refute(File.exist?(File.join(workspace, 'work', slug)))
      end
    end
  end

  def test_session_backend_rechecks_after_waiting_for_runtime_lock
    with_workspace do |workspace|
      root = File.join(workspace, '.test-user-state')
      runner = runner_for(workspace, env: {'DEV_WORKSPACES_STATE' => root})
      slug = '2026-11-12-pi'
      original = runner.method(:with_host_lock)
      runner.define_singleton_method(:with_host_lock) do |*args, **options, &block|
        receipt = WorkspaceSessionBackends.creation_paths(root, workspace, slug).first
        FileUtils.mkdir_p(File.dirname(receipt), mode: 0700)
        File.write(receipt, JSON.generate('schema' => 4), mode: 'w', perm: 0600)
        original.call(*args, **options, &block)
      end
      error = assert_raises(DevSession::Error) { runner.send(:with_slug_lock, slug) { flunk('stale admission entered') } }
      assert_includes(error.message, 'schema-4 validation')
    end
  end

  def test_tracking_lifecycle_conversion_preserves_explicit_and_strict_policies
    runner = DevSession::Runner.allocate
    %w[active complete abandoned archived].each do |source|
      %w[active complete abandoned archived].each do |target|
        ["\n", "\r\n"].each do |newline|
          body = "# State#{newline}#{newline}lifecycle: active#{newline}"
          content = "---#{newline}lifecycle: #{source}#{newline}---#{newline}#{body}"
          if source == target
            assert_same(content, runner.send(:tracking_lifecycle_content, content, target))
          elsif source == 'active' || target == 'archived'
            expected = "---#{newline}lifecycle: #{target}#{newline}---#{newline}#{body}"
            assert_equal(expected, runner.send(:tracking_lifecycle_content, content, target))
          else
            error = assert_raises(DevSession::Error) do
              runner.send(:tracking_lifecycle_content, content, target)
            end
            assert_equal("initiative lifecycle is #{source}; cannot archive it as #{target}", error.message)
          end
        end
      end
    end
  end

  def test_tracking_lifecycle_conversion_rejects_invalid_anchored_headers
    runner = DevSession::Runner.allocate
    [
      "# State\n\n---\nlifecycle: complete\n---\n",
      "---\nlifecycle: unknown\n---\n",
      "---\nlifecycle: complete\nowner: lead\n---\n",
      " \n---\nlifecycle: complete\n---\n"
    ].each do |content|
      assert_raises(DevSession::Error) do
        runner.send(:tracking_lifecycle_content, content, 'archived')
      end
    end
  end

  def test_tracking_commits_serialize_on_fresh_head_and_preserve_unrelated_index_with_hooks
    with_workspace do |workspace|
      slugs = %w[first-tracking second-tracking]
      runner = runner_for(workspace)
      slugs.each { |slug| runner.ensure_tracking_files(slug) }
      commit_tracking(workspace, slugs.first, lifecycle: 'active')
      assert_git_success('git', '-C', workspace, 'add', File.join('work', slugs.last))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'second tracking')
      configure_workspace_origin(workspace)
      initial = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip
      File.write(File.join(workspace, 'unrelated.txt'), 'staged sentinel')
      assert_git_success('git', '-C', workspace, 'add', 'unrelated.txt')
      File.write(File.join(workspace, 'unrelated.txt'), 'working sentinel')
      slugs.each do |slug|
        set_lifecycle(workspace, slug, 'complete')
        FileUtils.mkdir_p(File.join(workspace, 'archive'))
        FileUtils.mv(File.join(workspace, 'work', slug), File.join(workspace, 'archive', slug))
      end
      first_entered, first_release, second_validated = Queue.new, Queue.new, Queue.new
      first = runner_for(workspace)
      first_owner = first.method(:validate_tracking_transition_commit!)
      first.define_singleton_method(:validate_tracking_transition_commit!) do |*args, **options|
        first_owner.call(*args, **options)
        first_entered << true
        raise "tracking fixture release timed out" unless first_release.pop(timeout: 5)
      end
      second = runner_for(workspace)
      second_owner = second.method(:validate_tracking_transition_commit!)
      second.define_singleton_method(:validate_tracking_transition_commit!) do |*args, **options|
        second_validated << git_capture(['git', '-C', workspace, 'rev-parse', 'HEAD']).strip
        second_owner.call(*args, **options)
      end
      # A real hook independently proves both commits execute inside the same
      # short common-directory lock, while unrelated paths remain outside them.
      marker = File.join(workspace, '.git', 'tracking-hooks')
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      lock_path = File.join(workspace, '.git', 'dev-session-tracking.lock')
      File.write(hook, <<~SH)
        #!/bin/sh
        #{Shellwords.escape(RbConfig.ruby)} -e 'File.open(ARGV[0], File::RDWR) { |f| exit(f.flock(File::LOCK_EX | File::LOCK_NB) ? 1 : 0) }' #{Shellwords.escape(lock_path)} || exit 1
        echo ran >> #{Shellwords.escape(marker)}
      SH
      File.chmod(0o755, hook)
      first_thread = Thread.new { first.send(:commit_tracking_transition!, slugs.first, direction: 'archive', mode: 'complete') }
      unless first_entered.pop(timeout: 5)
        first_thread.value unless first_thread.alive?
        flunk("first tracking commit did not enter validation: #{first_thread.backtrace}")
      end
      second_thread = Thread.new { second.send(:commit_tracking_transition!, slugs.last, direction: 'archive', mode: 'complete') }
      sleep 0.02
      assert_raises(ThreadError) { second_validated.pop(true) }
      first_release << true
      assert(first_thread.join(5), "first tracking commit did not finish: #{first_thread.backtrace}")
      first_thread.value
      second_head = second_validated.pop(timeout: 5)
      unless second_head
        second_thread.value unless second_thread.alive?
        flunk("second tracking commit did not enter validation: #{second_thread.backtrace}")
      end
      refute_equal(initial, second_head)
      assert(second_thread.join(5), "second tracking commit did not finish: #{second_thread.backtrace}")
      second_thread.value
      assert_equal('2', git_capture_success('git', '-C', workspace, 'rev-list', '--count', "#{initial}..HEAD").strip)
      assert_equal(2, File.readlines(marker).length)
      assert_equal('unrelated.txt', git_capture_success('git', '-C', workspace, 'diff', '--cached', '--name-only').strip)
      assert_equal('staged sentinel', git_capture_success('git', '-C', workspace, 'show', ':unrelated.txt'))
      assert_equal('working sentinel', File.read(File.join(workspace, 'unrelated.txt')))
      slugs.each { |slug| first.send(:require_clean_committed_tracking_transition!, slug, direction: 'archive') }
      assert_equal(0o600, File.stat(lock_path).mode & 0o777)
      FileUtils.rm_rf(File.join(workspace, 'archive', slugs.first))
      first.send(:commit_removed_tracking!, slugs.first, 'git_tracked' => true, 'tracking' => 'archive')
      assert_equal(3, File.readlines(marker).length)
      refute(first.send(:committed_path?, 'HEAD', File.join('archive', slugs.first)))
      assert_equal('unrelated.txt', git_capture_success('git', '-C', workspace, 'diff', '--cached', '--name-only').strip)

    ensure
      first_release << true if first_thread&.alive?
      [first_thread, second_thread].compact.each do |thread|
        thread.kill.join unless thread.join(5)
      end
    end
  end

  def test_ruby_manifest_validator_accepts_shared_fixture
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      directory = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(directory)
      FileUtils.cp(
        File.expand_path('../fixtures/portal-manifest-valid.yml', __dir__),
        File.join(directory, 'portal.yml')
      )

      manifest = runner_for(workspace).send(
        :load_portal_manifest,
        File.join(directory, 'portal.yml'),
        required: true
      )
      assert_equal(slug, manifest['slug'])
      assert_equal('ready', manifest.dig('creation', 'state'))
      refute(manifest.key?('tmux'))
      assert_equal('2026-09-03T12:00:00Z', manifest['finalized_at'])
    end
  end

  def test_ruby_manifest_validator_accepts_all_shared_valid_fixtures
    fixtures = Dir[File.expand_path('../fixtures/portal-manifest-valid*.yml', __dir__)]
    assert_operator(fixtures.length, :>=, 2)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        slug = '2026-09-03-example'
        directory = File.join(workspace, 'work', slug)
        FileUtils.mkdir_p(directory)
        FileUtils.cp(fixture, File.join(directory, 'portal.yml'))

        manifest = runner_for(workspace).send(
          :load_portal_manifest,
          File.join(directory, 'portal.yml'),
          required: true
        )
        assert_equal(slug, manifest['slug'], File.basename(fixture))
      end
    end
  end

  def test_duplicate_artifacts_preserve_readiness_and_manifest_bytes
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      path = File.join(workspace, 'work', slug, 'portal.yml')
      original = File.binread(
        File.expand_path('../fixtures/portal-manifest-valid-duplicate-artifacts.yml', __dir__)
      )
      File.binwrite(path, original)
      manifest = runner.send(:load_portal_manifest, path, required: true)
      before = Marshal.load(Marshal.dump(manifest))
      assert_equal(manifest, runner.send(:require_ordinary_manifest_ready!, slug, manifest))
      runner.send(:validate_portal_manifest!, manifest, slug, path)
      runner.validate
      assert_equal(before, manifest)
      assert_equal(original, File.binread(path))
      assert_equal('thread-1', manifest.dig('codex', 'thread_id'))
      assert_equal(5, manifest.fetch('artifacts').length)

      runner.send(:write_portal_manifest, slug, manifest)
      assert_equal(before, runner.send(:load_portal_manifest, path, required: true))
      manifest.fetch('artifacts') << { 'label' => '', 'path' => 'report.json' }
      saved = File.binread(path)
      assert_raises(DevSession::Error) { runner.send(:write_portal_manifest, slug, manifest) }
      assert_equal(saved, File.binread(path))
    end
  end

  def test_validate_checks_all_persisted_portal_manifests
    with_workspace do |workspace|
      valid_directory = File.join(workspace, 'work', '2026-09-03-example')
      invalid_directory = File.join(workspace, 'archive', '2026-09-03-invalid')
      FileUtils.mkdir_p(valid_directory)
      FileUtils.mkdir_p(invalid_directory)
      FileUtils.cp(
        File.expand_path('../fixtures/portal-manifest-valid.yml', __dir__),
        File.join(valid_directory, 'portal.yml')
      )
      File.write(
        File.join(invalid_directory, 'portal.yml'),
        "schema: 1\nslug: 2026-09-03-other\n"
      )

      assert_raises(DevSession::Error) do
        runner_for(workspace).validate
      end
    end
  end

  def test_validate_enforces_tracking_lifecycle_and_duplicate_placement
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      File.write(
        File.join(workspace, 'work', slug, 'portal.yml'),
        "schema: 1\nslug: #{slug}\nrepositories: []\nartifacts: []\n"
      )
      out = StringIO.new
      validating_runner = runner_for(workspace, out:)
      validating_runner.validate
      assert_includes(out.string, 'validated 1 portal manifest')

      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Plan\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n"
      )
      File.write(
        File.join(archive, 'portal.yml'),
        "schema: 1\nslug: #{slug}\nfinalized_at: '2026-09-03T12:00:00Z'\nrepositories: []\nartifacts: []\n"
      )
      error = assert_raises(DevSession::Error) do
        validating_runner.validate
      end
      assert_includes(error.message, 'duplicate session')

      FileUtils.rm_rf(File.join(workspace, 'work', slug))
      File.write(File.join(archive, 'state.md'), "---\nlifecycle: active\n---\n")
      error = assert_raises(DevSession::Error) do
        validating_runner.validate
      end
      assert_includes(error.message, 'terminal lifecycle')
    end
  end

  def test_ruby_manifest_validator_rejects_shared_invalid_fixtures
    fixtures = Dir[File.expand_path('../fixtures/portal-manifest-invalid-*.yml', __dir__)]
    refute_empty(fixtures)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        slug = '2026-09-03-example'
        directory = File.join(workspace, 'work', slug)
        FileUtils.mkdir_p(directory)
        FileUtils.cp(fixture, File.join(directory, 'portal.yml'))

        assert_raises(DevSession::Error, File.basename(fixture)) do
          runner_for(workspace).send(
            :load_portal_manifest,
            File.join(directory, 'portal.yml'),
            required: true
          )
        end
      end
    end
  end

  def test_ruby_runtime_authority_validator_uses_shared_corpus
    runner = runner_for('/tmp')
    Dir[File.expand_path('../fixtures/runtime-authority-valid-*.json', __dir__)].each do |fixture|
      record = JSON.parse(File.read(fixture))
      record['workspace'] = runner.workspace
      runner.send(:validate_session_authority!, record, 'example', fixture)
    end
    Dir[File.expand_path('../fixtures/runtime-authority-invalid-*.json', __dir__)].each do |fixture|
      record = JSON.parse(File.read(fixture))
      record['workspace'] = runner.workspace
      assert_raises(DevSession::Error, File.basename(fixture)) do
        runner.send(:validate_session_authority!, record, 'example', fixture)
      end
    end
  end

  def test_exclusive_start_refuses_to_reuse_an_existing_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          exclusive: true
        )
      end

      assert_match(/already exists/, error.message)
    end
  end

  def test_tracking_files_refuse_an_archived_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'archive', slug))

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_a_dangling_archive_symlink
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'archive'))
      FileUtils.ln_s(
        File.join(workspace, 'missing-archive-target'),
        File.join(workspace, 'archive', slug)
      )

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_a_slug_found_only_in_archive_history
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Plan\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n\n# #{slug}\n\n## Status\n"
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('archive', slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'archive initiative')
      FileUtils.rm_r(archive)
      assert_git_success('git', '-C', workspace, 'add', '-A', '--', File.join('archive', slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'remove archive checkout')
      assert_equal(
        '',
        git_capture_success('git', '-C', workspace, 'ls-files', '--', File.join('archive', slug))
      )

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_a_slug_found_only_in_the_git_index
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Plan\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n\n# #{slug}\n\n## Status\n"
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('archive', slug))
      FileUtils.rm_r(archive)
      refute_empty(
        git_capture_success('git', '-C', workspace, 'ls-files', '--', File.join('archive', slug))
      )

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_file_creation_does_not_execute_workspace_git_configuration
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      assert_git_success('git', 'init', '-b', 'master', workspace)
      marker = File.join(workspace, 'git-hook-ran')
      hook = File.join(workspace, 'hostile-fsmonitor')
      File.write(hook, "#!/bin/sh\ntouch #{Shellwords.escape(marker)}\nprintf '{}\\n'\n")
      FileUtils.chmod(0o755, hook)
      assert_git_success('git', '-C', workspace, 'config', 'core.fsmonitor', hook)

      runner_for(workspace).ensure_tracking_files(slug)

      refute(File.exist?(marker))
      assert(File.file?(File.join(workspace, 'work', slug, 'plan.md')))
    end
  end

  def test_tracking_files_fail_closed_when_archive_history_cannot_be_read
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      File.write(File.join(workspace, '.git'), "not a git directory\n")

      assert_raises(DevSession::CommandError) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_an_existing_empty_partial_write
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      directory = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(directory)
      File.write(File.join(directory, 'plan.md'), '')

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/empty or truncated/, error.message)
    end
  end

  def test_list_output_uses_aligned_columns_for_long_slugs
    with_workspace do |workspace|
      short_slug = '2026-06-06-short'
      long_slug = '2026-06-06-this-is-a-longer-development-session-name'

      FileUtils.mkdir_p(File.join(workspace, 'work', short_slug))
      FileUtils.mkdir_p(File.join(workspace, 'work', long_slug))

      worktree = File.join(workspace, 'worktrees', long_slug, 'alpha')
      FileUtils.mkdir_p(worktree)
      File.write(File.join(worktree, '.git'), "gitdir: /tmp/nonexistent\n")

      out = StringIO.new
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY
      )

      runner.list

      lines = out.string.lines.map(&:chomp)
      header = lines.fetch(0)
      work_column = header.index('WORK')
      worktrees_column = header.index('WORKTREES')
      tmux_column = header.index('TMUX')

      assert_equal(3, lines.length)

      lines.drop(1).each do |line|
        assert_equal('yes', line[work_column, 3])
        assert_match(/[0-9]/, line[worktrees_column, 'WORKTREES'.length])
        assert_equal('none', line[tmux_column, 4])
      end
    end
  end

  def test_current_uses_environment_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      runner = runner_for(
        workspace,
        env: {
          DevSession::ENV_SLUG => slug,
          DevSession::ENV_WORKSPACE => workspace
        },
        out:
      )

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_rejects_an_environment_from_another_workspace
    with_workspace do |workspace|
      Dir.mktmpdir('foreign-dev-session-workspace') do |foreign_workspace|
        runner = runner_for(
          workspace,
          env: {
            DevSession::ENV_SLUG => '2026-06-06-demo',
            DevSession::ENV_WORKSPACE => foreign_workspace
          }
        )

        error = assert_raises(DevSession::Error) { runner.current }

        assert_match(/not managed by this workspace/, error.message)
      end
    end
  end

  def test_current_uses_managed_tmux_session_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      runner = runner_for(
        workspace,
        tmux: CurrentTmux.new(slug, workspace:),
        env: { 'TMUX' => 'socket', 'TMUX_PANE' => '%1' },
        out:
      )

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_rejects_a_tmux_session_from_another_workspace
    with_workspace do |workspace|
      Dir.mktmpdir('foreign-dev-session-workspace') do |foreign_workspace|
        tmux = CurrentTmux.new('2026-06-06-demo', workspace: foreign_workspace)
        runner = runner_for(
          workspace,
          tmux:,
          env: { 'TMUX' => 'socket', 'TMUX_PANE' => '%1' }
        )

        error = assert_raises(DevSession::Error) { runner.current }

        assert_match(/not managed by this workspace/, error.message)
      end
    end
  end

  def test_current_uses_work_directory_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cwd = File.join(workspace, 'work', slug, 'notes')
      FileUtils.mkdir_p(cwd)

      runner = runner_for(workspace, cwd:)

      assert_equal(slug, runner.current_slug)
    end
  end

  def test_current_uses_worktrees_directory_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cwd = File.join(workspace, 'worktrees', slug, 'alpha', 'app')
      FileUtils.mkdir_p(cwd)

      runner = runner_for(workspace, cwd:)

      assert_equal(slug, runner.current_slug)
    end
  end

  def test_current_reports_missing_active_session
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(DevSession::Error) do
        runner.current
      end

      assert_match(/no current dev session/, error.message)
    end
  end

  def test_current_ignores_tmux_server_current_session_outside_a_tmux_pane
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        cwd: workspace
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      error = assert_raises(DevSession::Error) { runner.current }

      assert_match(/no current dev session/, error.message)
      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_current_reports_conflicting_sources
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      other_slug = '2026-06-06-other'
      cwd = File.join(workspace, 'work', other_slug)
      FileUtils.mkdir_p(cwd)

      runner = runner_for(
        workspace,
        env: {
          DevSession::ENV_SLUG => slug,
          DevSession::ENV_WORKSPACE => workspace
        },
        cwd:
      )

      error = assert_raises(DevSession::Error) do
        runner.current
      end

      assert_match(/sources disagree/, error.message)
      assert_match(/#{DevSession::ENV_SLUG}=#{slug}/, error.message)
      assert_match(/cwd=#{other_slug}/, error.message)
    end
  end

end
