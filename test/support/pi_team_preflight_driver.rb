# frozen_string_literal: true

# Owned disposable cross-language fixture. The predecessor harness, when given,
# remains its archived source; only this driver and synthetic records are new.
source = ENV.fetch('P2_TEAM_SOURCE_ROOT')
old = ENV['P2_TEAM_PREDECESSOR_ROOT']
selected_test = if ENV['P2_PI_PREPARATION_INPUT']
  'test_pi_preparation_version_transition'
elsif ENV['P2_PI_TRANSITION_INPUT']
  'test_pi_profile_transition_original'
else
  'test_pi_team_actual_scanner_retained_fence'
end
ARGV.replace(['--name', selected_test])
require File.join(old || source, 'test/workspace_host_test')
require File.join(source, 'libexec/workspace-session-backends')
require File.join(source, 'test/support/pi_creation_fixture')

class WorkspaceHostTest
  include PiCreationFixture

  def test_pi_preparation_version_transition
    input = JSON.parse(File.read(ENV.fetch('P2_PI_PREPARATION_INPUT')))
    with_transition_host do |prototype, paths|
      registry = prototype.send(:registry)
      registry.unregister('example-workspace')
      registry.register(name: 'example-workspace', root: input.fetch('workspace'), hostname: 'fixture.workspace.example.test', aliases: [], replace: false)
      old = make_package(paths.fetch(:root), 'preparation-v2')
      target = make_package(paths.fetch(:root), 'preparation-v3')
      contract = JSON.parse(File.read(File.join(old, 'share/workspace-portal/runtime-contract.json')))
        .merge(WorkspaceSessionBackends::FIELDS).merge(WorkspaceSessionBackends::WORKFLOW_FIELDS)
        .merge('piTeamRecords' => WorkspaceSessionBackends::TEAM_FIELDS, 'piRoleInstructions' => 1)
      [old, target].each do |package|
        File.write(File.join(package, 'share/workspace-portal/runtime-contract.json'), JSON.generate(contract.merge('piPreparationSchema' => package == old ? 2 : 3)))
      end
      environment = prototype.instance_variable_get(:@env).merge('DEV_WORKSPACES_STATE' => input.fetch('state'), 'DEV_WORKSPACES_PROFILE' => input.fetch('profile'))
      host = TransitionHost.new(candidate: target, busy: [], env: environment, out: StringIO.new, err: StringIO.new)
      helper = ENV.fetch('P2_TEAM_PORTAL_HELPER')
      host.define_singleton_method(:run_pi_team_preflight) do |roots|
        output, error, status = Open3.capture3(helper, 'pi-team-preflight', '--state-root', @state, *roots.flat_map { |root| ['--workspace', root] })
        raise DevWorkspaceHost::Error, error unless status.success?
        output
      end
      FileUtils.mkdir_p(File.dirname(input.fetch('profile')))
      File.symlink(old, input.fetch('profile') + '-1-link')
      File.symlink(File.basename(input.fetch('profile')) + '-1-link', input.fetch('profile'))
      before = File.binread(input.fetch('legacy'))
      legacy = JSON.parse(host.send(:run_pi_team_preflight, [input.fetch('workspace')]))
      assert_equal(2, legacy.fetch('schema'))
      refute(legacy.fetch('piPreparations').first.key?('schema'))
      host.send(:require_compatible_session_backends!, old)
      # The candidate owns the first install: the old exact-version Ruby gate
      # cannot certify target 3. The existing native/activation seams stay fake.
      host.executing_package = target
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']), host.instance_variable_get(:@err).string)
      assert_equal(target, File.realpath(input.fetch('profile')))
      assert_equal(before, File.binread(input.fetch('legacy')))
      FileUtils.cp(input.fetch('newSource'), input.fetch('newDestination'))
      File.chmod(0600, input.fetch('newDestination'))
      current = JSON.parse(host.send(:run_pi_team_preflight, [input.fetch('workspace')]))
      assert_equal(3, current.fetch('schema'))
      assert_equal([2, 3], current.fetch('piPreparations').map { |ref| ref.fetch('schema') }.sort)
      host.send(:require_compatible_session_backends!, target)
      error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, old) }
      assert_includes(error.message, 'retained Pi preparation')
      host.events.clear
      token = host.send(:profile_link_token)
      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(token, host.send(:profile_link_token))
      refute(host.events.any? { |event| %i[profile_set profile_selected profile_rolled_back configured sessions_quiesced consumers_restarted].include?(event.first) })
      assert_equal(before, File.binread(input.fetch('legacy')))
      assert_equal(File.binread(input.fetch('newSource')), File.binread(input.fetch('newDestination')))
      puts JSON.generate('preparationTransition' => '2-to-3', 'rollback' => 'refused', 'retainedVersions' => [2, 3])
    end
  end

  def test_pi_profile_transition_original
    input = JSON.parse(File.read(ENV.fetch('P2_PI_TRANSITION_INPUT')))
    input['slug'] ||= File.basename(input.fetch('frozenPaths').find { |path| path.end_with?('.creation.json') }, '.creation.json')
    if %w[activate prepared].include?(ENV['P2_PI_TRANSITION_PHASE'])
      pi_original_transition_host(input) do |host|
        arguments = ENV['P2_PI_TRANSITION_PHASE'] == 'activate' ? ['_activate'] : ['activate-prepared', '--artifact', 'fixture-artifact', '--attempt', '11111111-1111-4111-8111-111111111111']
        assert_equal(0, host.run('workspace-host', arguments), host.instance_variable_get(:@err).string)
        refute(host.events.any? { |event| %i[sessions_quiesced consumers_restarted].include?(event.first) })
      end
      return
    end
    with_transition_host do |prototype, paths|
      registry = prototype.send(:registry)
      registry.unregister('example-workspace')
      registry.register(name: 'example-workspace', root: input.fetch('workspace'), hostname: 'fixture.workspace.example.test', aliases: [], replace: false)
      input['environment'] = prototype.instance_variable_get(:@env).merge(
        'DEV_WORKSPACES_STATE' => input.fetch('state'), 'DEV_WORKSPACES_PROFILE' => input.fetch('profile'),
        'DEV_WORKSPACES_SYSTEM_CODEX' => File.join(input.fetch('a'), 'libexec/codex/bin/codex'),
        'DEV_WORKSPACE_CLAIM_ID' => 'fixture-held-claim')
      input['source'] = paths.fetch(:source)
      input['trace'] = File.join(paths.fetch(:root), 'transition-trace.jsonl')
      pi_original_transition_host(input) do |host|
        entry = host.send(:registry).entries.first
        codex = File.realpath(host.send(:codex_for_package, input.fetch('a')))
        host.send(:root_codex, codex, File.join(input.fetch('state'), 'codex/current'))
        plan = host.send(:registration_plan_for, entry, package: input.fetch('a'))
        host.send(:write_registration_marker, entry, codex, plan)
        before = input.fetch('frozenPaths').to_h { |path| [path, File.binread(path)] }
        host.executing_package = input.fetch('b')
        File.write(ENV.fetch('P2_PI_TRANSITION_INPUT'), JSON.generate(input))
        if input.fetch('mode', 'direct') == 'direct'
          # A retained native-domain claim still requires the existing ready
          # creation fence, even though the stopped receipt itself is coherent.
          receipt = JSON.parse(File.binread(WorkspaceSessionBackends.creation_paths(input.fetch('state'), input.fetch('workspace'), input.fetch('slug')).first))
          namespace = "#{File.basename(input.fetch('workspace'))}-#{Digest::SHA256.hexdigest(input.fetch('workspace'))[0, 16]}"
          roster = File.join(input.fetch('state'), 'codex-teams', namespace, input.fetch('slug') + '.json')
          now = Time.now.utc.iso8601
          record = {'schema' => 2, 'workspace' => input.fetch('workspace'), 'slug' => input.fetch('slug'), 'revision' => 1,
            'createdAt' => now, 'updatedAt' => now, 'backend' => 'pi', 'backendContract' => 1,
            'sessionIncarnation' => receipt.fetch('pi').fetch('sessionIncarnation'),
            'leadIncarnation' => receipt.fetch('pi').fetch('conversations').first.fetch('incarnation'), 'members' => []}
          FileUtils.mkdir_p(File.dirname(roster), mode: 0700)
          File.write(roster, JSON.generate(record), mode: 'w', perm: 0600)
          token = host.send(:profile_link_token)
          assert_equal(1, host.run('workspace-host', ['switch', '--source', input.fetch('source'), '--from-candidate']))
          assert_equal(token, host.send(:profile_link_token))
          refute(host.events.any? { |event| %i[profile_set configured sessions_quiesced consumers_restarted].include?(event.first) })
          File.unlink(roster)
        end
        case input.fetch('mode', 'direct')
        when 'prepared'
          input['prepared'] = JSON.parse(JSON.generate(host.send(:prepare_profile, ['--source', input.fetch('source'), '--from-candidate'], from_candidate: true)))
          File.write(ENV.fetch('P2_PI_TRANSITION_INPUT'), JSON.generate(input))
          output, error, status = Open3.capture3(ENV.to_h.merge('P2_PI_TRANSITION_PHASE' => 'prepared'), RbConfig.ruby, File.join(ENV.fetch('P2_TEAM_SOURCE_ROOT'), 'test/support/pi_team_preflight_driver.rb'))
          assert(status.success?, output + error)
        when 'forward'
          host.fail_set_after_profile = true
          assert_equal(1, host.run('workspace-host', ['switch', '--source', input.fetch('source'), '--from-candidate']))
          assert_equal(input.fetch('b'), File.realpath(input.fetch('profile')))
          assert_equal(input.fetch('b'), host.send(:pending_codex_update).fetch('package_root'))
          before.each { |path, bytes| assert_equal(bytes, File.binread(path)) }
          assert_equal(0, host.run('workspace-host', ['switch', '--source', input.fetch('source')]), host.instance_variable_get(:@err).string)
        else
          assert_equal(0, host.run('workspace-host', ['switch', '--source', input.fetch('source'), '--from-candidate']), host.instance_variable_get(:@err).string)
        end
        assert_equal(input.fetch('b'), File.realpath(input.fetch('profile')))
        assert_nil(host.send(:pending_codex_update))
        before.each do |path, bytes|
          if input['mode'] == 'advanced' && path == WorkspaceSessionBackends.creation_paths(input.fetch('state'), input.fetch('workspace'), input.fetch('slug')).first
            original = JSON.parse(bytes); current = JSON.parse(File.binread(path))
            assert_equal(original.fetch('attempt') + 1, current.fetch('attempt'))
            assert_equal(original.reject { |key, _| key == 'attempt' }, current.reject { |key, _| key == 'attempt' })
          else
            assert_equal(bytes, File.binread(path), path)
          end
        end
        refute(host.events.any? { |event| %i[sessions_quiesced consumers_restarted].include?(event.first) })
        trace = File.readlines(input.fetch('trace')).map { |line| JSON.parse(line) }
        assert(trace.any? { |row| row['phase'] == 'prepare' })
        assert(trace.any? { |row| row['phase'] == 'exclusive' })
        assert(trace.any? { |row| row['phase'] == 'activate' && row['pid'] != Process.pid })
        puts JSON.generate('transition' => 'preserved', 'phases' => trace.map { |row| row['phase'] }.uniq, 'nativeEffects' => 0)
      end
    end
  end

  def pi_original_transition_host(input)
    environment = input.fetch('environment').merge('DEV_WORKSPACE_ACTIVATION' => '1')
    host = TransitionHost.new(candidate: input.fetch('b'), busy: [], env: environment, out: StringIO.new, err: StringIO.new)
    host.executing_package = input.fetch('b')
    if ENV['P2_PI_TRANSITION_PHASE'] == 'prepared'
      # Only the external coordinator lookup is fake. Its serialized preparation
      # and the production activate-prepared routing/EX validation are real.
      actual_store = host.send(:coordination_store)
      store = Object.new
      store.define_singleton_method(:read) { {'jobs' => {'fixture' => {'attempt' => '11111111-1111-4111-8111-111111111111', 'ownerId' => 'fixture'}}, 'owners' => {'fixture' => {'workspaceName' => 'example-workspace', 'workspace' => input.fetch('workspace')}}} }
      store.define_singleton_method(:require_compatible!) { |package| actual_store.require_compatible!(package) }
      engine = Object.new
      engine.define_singleton_method(:artifact!) { |id| raise 'wrong artifact' unless id == 'fixture-artifact'; input.fetch('prepared') }
      engine.define_singleton_method(:attempt_path) { |*| File.join(File.dirname(input.fetch('trace')), 'absent-phase.json') }
      host.define_singleton_method(:coordination_store) { store }
      host.define_singleton_method(:coordination_engine) { |_| engine }
    end
    # Claim/native checks are external boundaries, as in the existing host
    # transition fixtures. The actual backend classifier/scanner stays intact.
    host.define_singleton_method(:require_coordination_profile_claim!) do |_candidate|
      raise DevWorkspaceHost::Error, 'fixture claim changed' unless @env['DEV_WORKSPACE_CLAIM_ID'] == 'fixture-held-claim'
      @events << [:claim_checked]
    end
    host.define_singleton_method(:coordination_check_codex) do |_candidate_host, codex|
      check_codex(codex)
    end
    # Reuse the reporting-aware external registration seam from live_switch_test.
    host.define_singleton_method(:registration_plan_for) do |entry, package: package_root|
      policy = JSON.parse(File.read(File.join(package, 'share/workspace-portal/runtime-contract.json'))).fetch('workspaceReportingPolicy')
      tools = ['report_to_lead']
      tools << 'session_communication' if policy >= 2
      tools << 'monitor_operation' if policy == 3
      values = [['command', File.join(package, 'bin/workspace-portal')],
        ['args', ['team-mcp', '--user-state-root', @state, '--workspace', entry.fetch('root'), '--host-profile', @profile]],
        ['required', true], ['enabled_tools', tools], ['tools.report_to_lead.approval_mode', 'approve']]
      values << ['tools.session_communication.approval_mode', 'approve'] if policy >= 2
      values << ['tools.monitor_operation.approval_mode', 'approve'] if policy == 3
      argv = values.flat_map { |key, value| ['-c', "mcp_servers.team_reporting.#{key}=#{JSON.generate(value)}"] }
      DevWorkspaceHost::RegistrationPlan.new(package_root: package, argv:, digest: 'a' * 64, policy: 1, required_native_child_threads: 0, states: [])
    end
    helper = ENV.fetch('P2_TEAM_PORTAL_HELPER')
    host.define_singleton_method(:run_pi_team_preflight) do |roots|
      output, error, status = Open3.capture3(helper, 'pi-team-preflight', '--state-root', @state, *roots.flat_map { |root| ['--workspace', root] })
      raise DevWorkspaceHost::Error, error unless status.success?
      output
    end
    host.define_singleton_method(:activate_installed) do |_command|
      environment = ENV.to_h.merge('P2_PI_TRANSITION_PHASE' => 'activate')
      output, error, status = Open3.capture3(environment, RbConfig.ruby, File.join(ENV.fetch('P2_TEAM_SOURCE_ROOT'), 'test/support/pi_team_preflight_driver.rb'))
      raise DevWorkspaceHost::Error, output + error unless status.success?
      @events << [:selected_activated]
    end
    host.define_singleton_method(:require_profile_transition_sessions!) do |package, **options|
      phase = if options[:selected]
        'activate'
      elsif @pi_fixture_exclusive
        'exclusive'
      else
        'prepare'
      end
      result = DevWorkspaceHost::Host.instance_method(:require_profile_transition_sessions!).bind_call(self, package, **options)
      File.open(input.fetch('trace'), 'a') { |file| file.puts JSON.generate('phase' => phase, 'pid' => Process.pid, 'preserved' => result.fetch(:preserved)) }
      result
    end
    host.define_singleton_method(:validate_candidate_preparation!) do |candidate, **options|
      if input['mode'] == 'advanced' && !@pi_fixture_advanced
        path = WorkspaceSessionBackends.creation_paths(@state, input.fetch('workspace'), input.fetch('slug')).first
        receipt = JSON.parse(File.read(path)); receipt['attempt'] += 1
        File.write(path, JSON.generate(receipt)); @pi_fixture_advanced = true
      end
      @pi_fixture_exclusive = true
      DevWorkspaceHost::Host.instance_method(:validate_candidate_preparation!).bind_call(self, candidate, **options)
    ensure
      @pi_fixture_exclusive = false
    end
    yield host
  end

  def test_pi_team_actual_scanner_retained_fence
    with_transition_host do |host, paths|
      workspace = host.send(:registry).entries.first.fetch('root')
      state = host.instance_variable_get(:@state)
      slug = '2026-11-12-workflow'
      records = pi_creation_records(workspace, slug:)
      native = '11111111-1111-4111-8111-111111111111'
      records[:journal]['native']['native_id'] = native
      records[:completion]['nativeId'] = native
      records[:binding]['conversations'][0]['native_id'] = native
      saved = write_pi_creation_records(state, workspace, records, receipt: false)
      File.unlink(saved.delete(:binding))
      namespace = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      roster_path = File.join(state, 'codex-teams', namespace, slug + '.json')
      now = Time.now.utc.iso8601
      roster = {'schema' => 2, 'workspace' => workspace, 'slug' => slug, 'revision' => 1,
        'createdAt' => now, 'updatedAt' => now, 'backend' => 'pi', 'backendContract' => 1,
        'sessionIncarnation' => 'a' * 64, 'leadIncarnation' => 'b' * 64,
        'members' => [{'address' => 'implementer0', 'role' => 'implementer', 'index' => 0,
          'state' => 'ready', 'addedAt' => now, 'incarnation' => 'c' * 64, 'creationId' => 'd' * 64,
          'initialInputId' => '2' * 32, 'model' => 'model', 'reasoningEffort' => 'high',
          'behavior' => 'implementer', 'purpose' => 'implementation', 'instructions' => 'Retained fixture instructions', 'access' => 'workspace_write'}]}
      FileUtils.mkdir_p(File.dirname(roster_path), mode: 0700)
      File.write(roster_path, JSON.generate(roster), mode: 'w', perm: 0600)
      saved[:roster] = roster_path
      backup = roster_path + '.backup'
      File.write(backup, "not JSON; synthetic ancillary bytes\n", mode: 'w', perm: 0600)
      backup_stat = File.lstat(backup)
      saved[:backup] = backup
      ref = {'Session' => {'Workspace' => workspace, 'Slug' => slug, 'Incarnation' => 'a' * 64},
        'Backend' => 'pi', 'Member' => 'lead', 'Incarnation' => 'b' * 64, 'NativeID' => native}
      namespace_hash = Digest::SHA256.hexdigest(workspace)
      %w[questions plans].each do |kind|
        file = File.join(state, 'pi-' + kind, namespace_hash, slug, 'a' * 64, 'lead', 'b' * 64, kind + '.json')
        record = {'schema' => 1, 'conversation' => ref, 'revision' => 0}
        record.merge!(kind == 'questions' ? {'nextSequence' => 1, 'questions' => []} : {'proposals' => [], 'acceptances' => []})
        FileUtils.mkdir_p(File.dirname(file), mode: 0700)
        File.write(file, JSON.generate(record), mode: 'w', perm: 0600)
        saved[kind.to_sym] = file
      end
      mapping = {'Schema' => 2, 'CreationID' => 'd' * 64, 'Conversation' => ref,
        'HistoryRef' => Digest::SHA256.hexdigest(JSON.generate(['pi-history', 1, 'pi', workspace, slug, 'a' * 64, 'lead', 'b' * 64, native])),
        'HistoryFile' => '', 'Generation' => 'e' * 64, 'HeldReason' => 'retained held worker',
        'Settings' => {'Provider' => 'native', 'AccountRef' => 'account', 'Model' => 'model', 'Thinking' => 'high', 'PolicyRevision' => 'f' * 64, 'Revision' => 1},
        'PendingSettings' => nil, 'Loaded' => false, 'Persisted' => false, 'WorkerPID' => 0, 'WorkerStart' => ''}
      mapping['Instructions'] = "Exact frozen rôle\n/path-looking-text"
      mapping['InitialSettings'] = mapping['Settings'].dup
      policy = {'Mode' => 'plan', 'Revision' => 1, 'Active' => ['read'], 'RegisteredCeiling' => ['read']}
      policy['Digest'] = Digest::SHA256.hexdigest(JSON.generate(['pi-tool-policy', 1, 'plan', ['read'], ['read']]))
      mapping['ToolPolicy'] = policy
      file = File.join(state, 'pi-runtime', namespace_hash, slug, 'a' * 64, 'members', 'lead', 'b' * 64, 'conversation.json')
      FileUtils.mkdir_p(File.dirname(file), mode: 0700)
      File.write(file, JSON.generate(mapping), mode: 'w', perm: 0600)
      saved[:mapping] = file
      original = saved.to_h { |key, path| [key, File.binread(path)] }
      contract_path = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
      contract = JSON.parse(File.read(contract_path)).merge(WorkspaceSessionBackends::FIELDS)
      contract['piTeamRecords'] = WorkspaceSessionBackends::TEAM_FIELDS
      contract.merge!(WorkspaceSessionBackends::WORKFLOW_FIELDS)
      contract['piRoleInstructions'] = 1
      File.write(contract_path, JSON.generate(contract))
      marker = File.join(paths.fetch(:root), 'scanner-called')
      reader = make_package(paths.fetch(:root), 'executing-reader')
      helper = File.join(reader, 'bin/workspace-portal')
      script = "#!/bin/sh\nprintf called >> #{marker.shellescape}\nexec #{ENV.fetch('P2_TEAM_PORTAL_HELPER').shellescape} \"$@\"\n"
      File.write(helper, script, mode: 'w', perm: 0700)
      host.executing_package = reader
      if ENV['P2_TEAM_PREDECESSOR_ROOT']
        # The actual old parser knows no scanner. Its recognized journal must
        # refuse both actual entry points even with cache and binding absent.
        candidate = host.candidate
        host.define_singleton_method(:package_root) { candidate }
        [false, true].each do |from_candidate|
          arguments = ['--source', paths.fetch(:source)]
          arguments << '--from-candidate' if from_candidate
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:prepare_profile, arguments, from_candidate:) }
          assert_match(/creation.*(schema|state)/, error.message)
          original.each { |key, bytes| assert_equal(bytes, File.binread(saved.fetch(key))) }
        end
        refute(File.exist?(marker), 'predecessor unexpectedly reached the new scanner')
      else
        host.define_singleton_method(:run_pi_team_preflight) do |roots|
          DevWorkspaceHost::Host.instance_method(:run_pi_team_preflight).bind_call(self, roots)
        end
        host.send(:require_compatible_session_backends!, host.candidate)
        assert(File.exist?(marker), 'actual Go scanner was not invoked')
        %w[cold held pending].each do |phase|
          value = Marshal.load(Marshal.dump(mapping))
          value['HeldReason'] = phase == 'held' ? 'retained held worker' : ''
          value['PendingToolPolicy'] = policy.merge('Revision' => 2) if phase == 'pending'
          File.write(saved.fetch(:mapping), JSON.generate(value), mode: 'w', perm: 0600)
          output = host.send(:run_pi_team_preflight, [workspace])
          claims = WorkspaceSessionBackends.team_preflight!(output, root: state, workspaces: [workspace]).fetch('sessions')
          rows = claims.select { |claim| %w[tool_policy role_instructions].include?(claim.fetch('source').fetch('kind')) }
          assert_equal(%w[role_instructions tool_policy], rows.map { |claim| claim['source']['kind'] }.sort)
          assert_equal(rows[0]['source']['id'], rows[1]['source']['id'])
          assert_equal(rows[0]['members'], rows[1]['members'])
          assert_equal('lead', rows[0]['members'][0]['address'])
          assert_equal('d' * 64, rows[0]['members'][0]['creationId'])
          refute(rows.any? { |claim| claim.key?('lead') })
          refute_includes(output, mapping.fetch('Instructions'))
          host.send(:require_compatible_session_backends!, host.candidate)
          assert_equal(JSON.generate(value).b, File.binread(saved.fetch(:mapping)))
        end
        File.write(saved.fetch(:mapping), original.fetch(:mapping), mode: 'w', perm: 0600)
        [nil, false, '1', 1.0, 0, 2].each do |version|
          File.write(contract_path, JSON.generate(contract.merge('piRoleInstructions' => version)))
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
          assert_match(/role instructions/, error.message)
        end
        File.write(contract_path, JSON.generate(contract.reject { |key, _| key == 'piRoleInstructions' }))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        File.write(contract_path, JSON.generate(contract))
        ['', nil].each do |instructions|
          value = mapping.merge('Instructions' => instructions)
          File.write(saved.fetch(:mapping), JSON.generate(value), mode: 'w', perm: 0600)
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
          assert_equal(JSON.generate(value).b, File.binread(saved.fetch(:mapping)))
        end
        File.write(saved.fetch(:mapping), original.fetch(:mapping), mode: 'w', perm: 0600)
        if ENV['P2_TEAM_PRIOR_TARGET_ROOT']
          prior = ENV.fetch('P2_TEAM_PRIOR_TARGET_ROOT')
          target = make_package(paths.fetch(:root), 'actual-prior-R')
          FileUtils.cp(File.join(prior, 'portal/internal/session/runtime-contract.json'), File.join(target, 'share/workspace-portal/runtime-contract.json'))
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, target) }
          assert_match(/role instructions/, error.message)
          command = [ENV.fetch('P2_TEAM_PRIOR_PORTAL_HELPER'), 'pi-team-preflight', '--state-root', state, '--workspace', workspace]
          _, stderr, status = Open3.capture3(*command)
          refute(status.success?, 'actual prior R reader accepted Instructions')
          assert_match(/Pi runtime scope mismatch/, stderr)
          legacy = mapping.reject { |key, _| key == 'Instructions' }
          File.write(saved.fetch(:mapping), JSON.generate(legacy), mode: 'w', perm: 0600)
          host.send(:require_compatible_session_backends!, target)
          stdout, stderr, status = Open3.capture3(*command)
          assert(status.success?, stderr)
          refute_includes(stdout, 'role_instructions')
          assert_equal(JSON.generate(legacy).b, File.binread(saved.fetch(:mapping)))
          File.write(saved.fetch(:mapping), original.fetch(:mapping), mode: 'w', perm: 0600)
        end
        original.each { |key, bytes| assert_equal(bytes, File.binread(saved.fetch(key))) }
        WorkspaceSessionBackends::WORKFLOW_FIELDS.each_key do |field|
          unsupported = contract.reject { |key, _| key == field }
          File.write(contract_path, JSON.generate(unsupported))
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        end
        contract.delete('piTeamRecords')
        File.write(contract_path, JSON.generate(contract))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        File.write(contract_path, JSON.generate(contract.merge('piTeamRecords' => WorkspaceSessionBackends::TEAM_FIELDS)))
        [:intent, :completion, :journal].each do |missing|
          bytes = File.binread(saved.fetch(missing))
          File.unlink(saved.fetch(missing))
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
          File.write(saved.fetch(missing), bytes, mode: 'w', perm: 0600)
        end
        pending = JSON.parse(original.fetch(:journal)).merge('state' => 'creating')
        File.write(saved.fetch(:journal), JSON.generate(pending), mode: 'w', perm: 0600)
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        File.write(saved.fetch(:journal), original.fetch(:journal), mode: 'w', perm: 0600)
        original.each { |key, bytes| assert_equal(bytes, File.binread(saved.fetch(key))) }
        File.unlink(saved.fetch(:journal))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
      end
      refute(File.exist?(host.instance_variable_get(:@profile)))
      refute_includes(host.events, [:sessions_quiesced])
      refute_includes(host.events, [:configured])
      refute(File.exist?(WorkspaceSessionBackends.binding_directory(state, workspace) + '/' + slug + '.json'))
      assert_equal("not JSON; synthetic ancillary bytes\n", File.binread(backup))
      assert_equal([backup_stat.ino, backup_stat.uid, backup_stat.mode, backup_stat.size, backup_stat.mtime, backup_stat.ctime],
        File.lstat(backup).then { |stat| [stat.ino, stat.uid, stat.mode, stat.size, stat.mtime, stat.ctime] })
    end
  end
end
