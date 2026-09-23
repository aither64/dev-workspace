# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  # This file must remain runnable by itself. The aggregate suite happens to
  # define a richer NullTmux later, but this focused journal test only needs
  # the absence of a live session.
  class InertTmux
    def session(_slug)
      nil
    end

    def argv(*arguments)
      ['tmux', *arguments]
    end
  end

  # Keep the publication/first-turn boundary independent from the aggregate
  # suite's tmux fixtures. The runner below exercises the real creation
  # journal and manifest ordering while this small command double models the
  # two durable outcomes exposed by `thread ensure-initial`: a materialized
  # matching initial request reconciles; an unmaterialized or different one
  # must not start another turn.
  class ManagedCreationScenario
    attr_accessor :crash_after_attempt_persistence, :materialized_goal,
                  :session
    attr_reader :ensure_commands, :events, :publication_requests, :turn_starts

    def initialize
      @ensure_commands = []
      @events = []
      @publication_requests = []
      @publication_outcomes = []
      @send_outcomes = []
      @turn_starts = 0
      @published = false
      @crashed_after_attempt_persistence = false
    end

    def queue_publication(*outcomes)
      @publication_outcomes.concat(outcomes)
    end

    def queue_send(*outcomes)
      @send_outcomes.concat(outcomes)
    end

    def publication_response(argv, input)
      request = JSON.parse(input)
      @publication_requests << request
      @events << :publication
      @published = true
      case @publication_outcomes.shift || :success
      when :success
        [JSON.generate(
          'schema' => 1,
          'state_revision' => 1,
          'identity' => {
            'workspace' => argv.fetch(argv.index('--workspace') + 1),
            'slug' => argv.fetch(argv.index('--slug') + 1),
            'root_thread_id' => request.fetch('root_thread_id'),
            'creation_identity' => request.fetch('creation_identity')
          }
        ), '', nil]
      when :response_lost
        raise DevSession::Error, 'managed publication response was lost'
      else
        raise "unknown publication outcome"
      end
    end

    def ensure_initial_response(argv)
      @ensure_commands << argv
      @events << :ensure_initial
      goal = File.read(argv.fetch(argv.index('--input-file') + 1))
      start_unmaterialized = argv.include?('--start-unmaterialized')

      if @materialized_goal
        unless @materialized_goal == goal
          raise DevSession::Error, 'materialized Codex thread has a different initial goal'
        end

        return ['', '', nil]
      end

      unless start_unmaterialized
        raise DevSession::Error,
              'initial request may already have been accepted by the unmaterialized Codex thread'
      end

      @turn_starts += 1
      case @send_outcomes.shift || :success
      when :success
        @materialized_goal = goal
        ['', '', nil]
      when :response_lost_unmaterialized
        raise DevSession::Error, 'initial turn response was lost'
      when :response_lost_materialized_exact
        @materialized_goal = goal
        raise DevSession::Error, 'initial turn response was lost after materialization'
      when :response_lost_materialized_different
        @materialized_goal = "different initial goal\n"
        raise DevSession::Error, 'initial turn response was lost after materialization'
      else
        raise 'unknown initial-turn outcome'
      end
    end

    def published?
      @published
    end

    def record_attempt_persistence!
      @events << :attempt_persisted
      return unless crash_after_attempt_persistence && !@crashed_after_attempt_persistence

      @crashed_after_attempt_persistence = true
      raise DevSession::Error, 'crash after managed publication before attempt persistence'
    end
  end

  class ManagedCreationCommandRunner
    def initialize(scenario)
      @scenario = scenario
    end

    def capture(argv, input: nil, **_options)
      case argv[1, 2]
      when ['agent-teams', 'publish-creation']
        @scenario.publication_response(argv, input)
      when ['thread', 'ensure-initial']
        @scenario.ensure_initial_response(argv)
      else
        raise "unexpected managed-creation command: #{argv.inspect}"
      end
    end
  end

  # The direct creation boundary calls the real roster helper before it asks
  # the lead thread to materialize its first turn. This small App Server
  # double makes that ordering and recovery contract observable without
  # simulating the runtime implementation itself.
  class DirectCreationScenario < ManagedCreationScenario
    attr_accessor :catalog_preset
    attr_reader :applied_presets, :member_threads, :root_settings

    def initialize
      super
      @applied_presets = []
      @member_threads = {}
    end

    def root_created!(model, effort)
      @events << :root_created
      @root_settings = [model, effort]
    end

    def apply_preset_response(argv)
      preset = JSON.parse(File.read(argv.fetch(argv.index('--preset-file') + 1)))
      @applied_presets << preset
      preset.fetch('members').each do |member|
        address = member.fetch('address')
        settings = [member.fetch('model'), member.fetch('reasoningEffort')]
        existing = @member_threads[address]
        if existing && existing != settings
          raise DevSession::Error, "direct retry changed #{address} settings"
        end
        @member_threads[address] ||= settings
      end
      @events << :members_ready
      [JSON.generate(
        'slug' => argv.fetch(argv.index('--session-slug') + 1),
        'rootThreadId' => argv.fetch(argv.index('--root-thread-id') + 1)
      ), '', nil]
    end

    def ensure_initial_response(argv)
      raise DevSession::Error, 'lead initial turn preceded direct member creation' unless @events.include?(:members_ready)

      super
    end
  end

  class DirectCreationCommandRunner
    attr_reader :commands

    def initialize(scenario)
      @scenario = scenario
      @commands = []
    end

    def capture(argv, input: nil, **_options)
      @commands << argv
      if argv[1] == 'team-preset'
        [JSON.generate(@scenario.catalog_preset), '', nil]
      elsif argv[1, 2] == ['team', 'apply-preset']
        @scenario.apply_preset_response(argv)
      elsif argv[1, 2] == ['thread', 'ensure-initial']
        @scenario.ensure_initial_response(argv)
      else
        raise "unexpected direct-creation command: #{argv.inspect}"
      end
    end
  end

  class ThreadCommandRunner
    attr_accessor :roster
    attr_reader :commands

    def initialize
      @commands = []
    end

    def capture(argv, **_options)
      @commands << argv
      return [JSON.generate('roster' => roster, 'presets' => []), '', nil] if argv[1, 2] == ['team', 'list']

      [JSON.generate('threadId' => 'thread-frozen'), '', nil]
    end
  end

  class ManagedCreationRunner < DevSession::Runner
    def initialize(*arguments, scenario:, **keywords)
      @scenario = scenario
      super(*arguments, **keywords)
    end

    private

    def select_tmux_for_slug!(_slug); end

    def cleanup_session(_slug)
      @scenario.session
    end

    def create_portal_thread(_slug, persisted_thread_id: nil, **_keywords)
      persisted_thread_id || 'thread-managed'
    end

    def name_portal_thread(_slug, _thread_id); end

    def verify_codex_client!; end

    def create_tmux_session(slug, run_codex:, thread_id:, launch_codex:, identity_token:)
      raise 'managed initial turn launched before persistence' if launch_codex
      raise 'managed initial thread was not created' unless run_codex && thread_id == 'thread-managed'

      @scenario.session ||= DevSession::Tmux::Session.new(
        id: '$8', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test/tmux.sock',
        codex_thread_id: thread_id, codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1',
        identity_token:
      )
    end

    def sync_slug(_slug, require_session:, session:)
      raise 'managed tmux session was not supplied' unless require_session && session

      session
    end

    def revalidate_session!(session)
      session
    end

    def ensure_managed_session!(_slug, session:)
      session
    end

    def quiesce_native_client!(_slug, session)
      session
    end

    def reconcile_native_client!(_slug, session, **_keywords)
      session
    end

    def write_portal_manifest(slug, manifest)
      @scenario.record_attempt_persistence! if manifest.dig('creation', 'initial_goal_attempted')
      super
    end
  end

  class DirectCreationRunner < ManagedCreationRunner
    private

    def create_portal_thread(_slug, persisted_thread_id: nil, model:, effort:, **_keywords)
      @scenario.root_created!(model, effort)
      persisted_thread_id || 'thread-direct'
    end

    def create_tmux_session(slug, run_codex:, thread_id:, launch_codex:, identity_token:)
      raise 'direct lead turn launched before member creation' if launch_codex
      raise 'direct root thread was not created' unless run_codex && thread_id == 'thread-direct'

      @scenario.session ||= DevSession::Tmux::Session.new(
        id: '$9', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test/tmux.sock',
        codex_thread_id: thread_id, codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1', identity_token:
      )
    end
  end

  def test_ready_managed_creation_journal_is_available_for_an_ordinary_restart
    with_workspace do |workspace|
      runner = managed_runner_for(workspace)
      slug = '2026-09-22-managed'
      path = runner.send(:creation_journal_file, slug)
      File.write(path, JSON.generate(managed_creation_journal(slug, state: 'ready')))
      File.chmod(0o600, path)

      assert_nil(runner.send(:recover_managed_creation!, slug, nil))
    end
  end

  def test_managed_creation_journal_rejects_duplicate_authority_keys
    with_workspace do |workspace|
      runner = managed_runner_for(workspace)
      slug = '2026-09-22-managed'
      path = runner.send(:creation_journal_file, slug)
      payload = JSON.generate(managed_creation_journal(slug, state: 'ready'))
      payload = payload.sub('{', '{"schema":2,')
      File.write(path, payload)
      File.chmod(0o600, path)

      assert_raises(DevSession::Error) { runner.send(:recover_managed_creation!, slug, nil) }
    end
  end

  def test_retired_virtual_team_binding_cannot_start_a_session
    with_workspace do |workspace|
      runner = managed_runner_for(workspace)
      error = assert_raises(DevSession::Error) do
        runner.start(
          '2026-09-22-retired-team', as_is: true, new: false, attach: false,
          run_codex: true, goal_text: 'Use independent team threads.', json: true,
          team_binding: "v1.eyJ4IjoieSJ9.#{'b' * 64}"
        )
      end
      assert_match(/virtual team creation is retired/, error.message)
    end
  end

  def test_direct_creation_builds_immutable_members_before_the_lead_goal_and_retries_without_duplicates
    with_workspace do |workspace|
      slug = '2026-09-22-direct-team'
      goal = File.join(workspace, 'direct-goal.txt')
      File.write(goal, "Start the independent team.\n")
      preset = direct_team_preset
      preset_path = File.join(workspace, 'direct-team.json')
      File.write(preset_path, JSON.generate(preset))
      File.chmod(0o600, preset_path)
      scenario = DirectCreationScenario.new
      scenario.queue_send(:response_lost_materialized_exact)
      runner = direct_creation_runner(workspace, scenario)

      assert_raises(DevSession::Error) { start_direct_creation(runner, slug, goal, preset_path) }
      retry_runner = direct_creation_runner(workspace, scenario)
      start_direct_creation(retry_runner, slug, goal, preset_path)

      assert_equal(['model-lead', 'high'], scenario.root_settings)
      assert_equal(
        {
          'architect0' => ['model-architect', 'medium'],
          'implementer0' => ['model-implementer', 'xhigh']
        },
        scenario.member_threads
      )
      assert_equal(2, scenario.applied_presets.length)
      assert_equal(scenario.applied_presets.fetch(0), scenario.applied_presets.fetch(1))
      assert_equal(['lead', 'architect0', 'implementer0'], scenario.applied_presets.fetch(0).fetch('roles'))
      root = scenario.events.index(:root_created)
      members = scenario.events.index(:members_ready)
      lead_goal = scenario.events.index(:ensure_initial)
      assert_operator(root, :<, members)
      assert_operator(members, :<, lead_goal)
      assert_equal(1, scenario.turn_starts)

      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal(3, journal.fetch('schema'))
      assert_equal(preset, journal.fetch('direct_team'))
      assert_equal('model-lead', journal.fetch('model'))
      assert_equal('high', journal.fetch('effort'))
    end
  end

  def test_prompt_bearing_direct_snapshot_and_thread_flags
    with_workspace do |workspace|
      slug = '2026-09-22-custom-team'
      preset = direct_team_preset
      preset['leadInstructions'] = "Coordinate the team.\nWait for the request."
      preset['roles'] = %w[lead scribe0]
      preset['members'] = [{
        'role' => 'scribe', 'address' => 'scribe0', 'behavior' => 'general',
        'access' => 'read_only', 'purpose' => 'general',
        'instructions' => 'Summarize the assignment.',
        'model' => 'model-scribe', 'reasoningEffort' => 'high'
      }]
      commands = ThreadCommandRunner.new
      portal = File.join(workspace, 'package', 'bin', 'workspace-portal')
      FileUtils.mkdir_p(File.dirname(portal))
      File.write(portal, "#!/bin/sh\n")
      File.chmod(0o755, portal)
      runner = DevSession::Runner.new(
        workspace:, authority_dir: File.join(workspace, 'runtime-authority'),
        tmux: InertTmux.new, out: StringIO.new, err: StringIO.new,
        today: Date.new(2026, 9, 22),
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
        cwd: workspace, portal_command: [portal], command_runner: commands
      )
      assert(runner.send(:valid_direct_team_preset?, preset))
      assert_equal('thread-frozen', runner.send(:create_portal_thread, slug,
        model: preset.fetch('leadModel'), effort: preset.fetch('leadEffort'),
        lead_instructions: preset.fetch('leadInstructions')))
      assert_equal('thread-frozen', runner.send(:create_portal_fork, slug, 'source-thread',
        model: preset.fetch('leadModel'), effort: preset.fetch('leadEffort'),
        lead_instructions: preset.fetch('leadInstructions')))
      commands.commands.each do |command|
        assert_equal(preset.fetch('leadInstructions'), command.fetch(command.index('--lead-instructions') + 1))
      end

      path = runner.send(:creation_journal_file, slug)
      journal = {
        'schema' => 3, 'slug' => slug, 'goal_sha256' => 'c' * 64,
        'run_codex' => true, 'state' => 'ready',
        'model' => preset.fetch('leadModel'), 'effort' => preset.fetch('leadEffort'),
        'direct_team' => preset
      }
      File.write(path, JSON.generate(journal))
      File.chmod(0o600, path)
      assert_equal(preset.fetch('leadInstructions'),
        runner.send(:frozen_direct_team_lead_instructions, slug, 'source-thread'))
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = { 'thread_id' => 'source-thread' }
      runner.send(:write_portal_manifest, slug, manifest)
      team_state = runner.send(:direct_team_state_path, slug)
      FileUtils.mkdir_p(File.dirname(team_state))
      File.write(team_state, "{}\n")
      commands.roster = {
        'workspace' => workspace, 'slug' => slug, 'rootThreadId' => 'source-thread',
        'leadInstructions' => 'Frozen from the source roster.'
      }
      assert_equal('Frozen from the source roster.',
        runner.send(:frozen_direct_team_lead_instructions, slug, 'source-thread'))
      preset['members'].first.delete('instructions')
      refute(runner.send(:valid_direct_team_preset?, preset))
    end
  end

  def test_cli_team_resolves_the_installed_direct_snapshot_without_hidden_lead_defaults
    with_workspace do |workspace|
      scenario = DirectCreationScenario.new
      scenario.catalog_preset = direct_team_preset
      package = File.join(workspace, 'package')
      portal = File.join(package, 'bin', 'workspace-portal')
      FileUtils.mkdir_p(File.dirname(portal))
      File.write(portal, "#!/bin/sh\n")
      File.chmod(0o755, portal)
      command_runner = DirectCreationCommandRunner.new(scenario)
      runner = DirectCreationRunner.new(
        workspace:,
        authority_dir: File.join(workspace, 'runtime-authority'),
        tmux: InertTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: Date.new(2026, 9, 22),
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
        cwd: workspace,
        portal_command: [portal],
        command_runner:,
        scenario:
      )

      resolved = runner.send(:resolve_current_direct_team!, 'delegated', model: nil, effort: nil)
      assert_equal(direct_team_preset, resolved)
      assert_equal(
        [portal, 'team-preset', '--package-root', package, '--team', 'delegated'],
        command_runner.commands.fetch(0)
      )
      error = assert_raises(DevSession::Error) do
        runner.send(:resolve_current_direct_team!, 'delegated', model: 'model-lead', effort: nil)
      end
      assert_match(/--model and --effort/, error.message)
      assert_equal(1, command_runner.commands.length)
    end
  end

  def test_cli_team_retry_keeps_frozen_snapshot_after_catalog_changes
    with_workspace do |workspace|
      slug = '2026-09-22-frozen-team-retry'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Start the team.\n")
      package = File.join(workspace, 'package')
      portal = File.join(package, 'bin', 'workspace-portal')
      FileUtils.mkdir_p(File.dirname(portal))
      File.write(portal, "#!/bin/sh\n")
      File.chmod(0o755, portal)
      scenario = DirectCreationScenario.new
      original = direct_team_preset
      original['leadInstructions'] = 'Original frozen lead instructions.'
      original['members'].each do |member|
        member['purpose'] = { 'designer' => 'design', 'implementer' => 'implementation' }.fetch(member['behavior'])
        member['instructions'] = "Original frozen #{member['role']} instructions."
      end
      scenario.catalog_preset = original
      scenario.queue_send(:response_lost_materialized_exact)
      create_runner = lambda do |command_runner|
        DirectCreationRunner.new(
          workspace:, authority_dir: File.join(workspace, 'runtime-authority'),
          tmux: InertTmux.new, out: StringIO.new, err: StringIO.new,
          today: Date.new(2026, 9, 22),
          env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
          cwd: workspace, portal_command: [portal], command_runner:, scenario:
        )
      end
      first_commands = DirectCreationCommandRunner.new(scenario)
      first = create_runner.call(first_commands)
      start = lambda do |runner, model|
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, team: 'delegated', model:, effort: 'high')
      end
      assert_raises(DevSession::Error) { start.call(first, 'model-lead') }
      assert(first_commands.commands.any? { |command| command[1] == 'team-preset' })

      changed = JSON.parse(JSON.generate(original))
      changed['leadInstructions'] = 'New catalog instructions.'
      changed['members'].first['instructions'] = 'New member instructions.'
      scenario.catalog_preset = changed
      retry_commands = DirectCreationCommandRunner.new(scenario)
      retry_runner = create_runner.call(retry_commands)
      error = assert_raises(DevSession::Error) { start.call(retry_runner, 'another-model') }
      assert_match(/selection does not match/, error.message)
      start.call(retry_runner, 'model-lead')

      refute(retry_commands.commands.any? { |command| command[1] == 'team-preset' })
      assert_equal([original, original], scenario.applied_presets)
      assert_equal(1, scenario.turn_starts)
    end
  end

  def test_cli_team_preset_uses_the_installed_direct_snapshot_instead_of_legacy_defaults
    with_workspace do |workspace|
      scenario = DirectCreationScenario.new
      scenario.catalog_preset = direct_team_preset
      package = File.join(workspace, 'package')
      portal = File.join(package, 'bin', 'workspace-portal')
      FileUtils.mkdir_p(File.dirname(portal))
      File.write(portal, "#!/bin/sh\n")
      File.chmod(0o755, portal)
      command_runner = DirectCreationCommandRunner.new(scenario)
      runner = DirectCreationRunner.new(
        workspace:,
        authority_dir: File.join(workspace, 'runtime-authority'),
        tmux: InertTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: Date.new(2026, 9, 22),
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
        cwd: workspace,
        portal_command: [portal],
        command_runner:,
        scenario:
      )
      slug = '2026-09-22-existing'
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = { 'thread_id' => 'existing-root' }
      manifest['creation'] = { 'state' => 'ready', 'initial_goal_sent' => true }
      runner.send(:write_portal_manifest, slug, manifest)

      result = runner.send(
        :invoke_team_command, slug, action: 'preset', preset: 'delegated',
        role: nil, address: nil, from: 'lead', to: nil, message: nil,
        model: nil, effort: nil, source_slug: nil, source_root_thread: nil
      )

      assert_equal(slug, JSON.parse(result).fetch('slug'))
      assert_equal(
        [portal, 'team-preset', '--package-root', package, '--team', 'delegated'],
        command_runner.commands.fetch(0)
      )
      apply = command_runner.commands.fetch(1)
	  assert_equal([portal, 'team', 'apply-preset'], apply.first(3))
      refute_includes(apply, '--preset')
      assert_equal(
        {
          'architect0' => ['model-architect', 'medium'],
          'implementer0' => ['model-implementer', 'xhigh']
        },
        scenario.member_threads
      )
    end
  end

  def test_member_cli_cannot_leave_its_roster_or_spoof_the_lead
    with_workspace do |workspace|
      calls = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:invoke_team_command) do |slug, **options|
          calls << [slug, options]
          'ok'
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: InertTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: Date.new(2026, 9, 22),
        env: {
          'XDG_STATE_HOME' => File.join(workspace, '.xdg-state'),
          DevSession::ENV_SLUG => '2026-09-22-own-session',
          DevSession::ENV_MEMBER_ADDRESS => 'architect0'
        },
        cwd: workspace,
        portal_command: ['workspace-portal-test']
      )

      error = assert_raises(DevSession::Error) do
        runner.team('2026-09-22-other-session', action: 'list')
      end
      assert_match(/own session roster/, error.message)
      error = assert_raises(DevSession::Error) do
        runner.team('2026-09-22-own-session', action: 'assign', from: 'lead', to: 'architect0', message: 'No.')
      end
      assert_match(/own address/, error.message)
      assert_empty(calls)

      assert_equal(
        'ok',
        runner.team('2026-09-22-own-session', action: 'assign', from: 'architect0', to: 'lead', message: 'Status.', lifecycle: true)
      )
      assert_equal(['2026-09-22-own-session', 'architect0'], [calls.fetch(0).fetch(0), calls.fetch(0).fetch(1).fetch(:from)])
    end
  end

  def test_team_cli_waits_for_direct_creation_proof_before_mutating_roster
    with_workspace do |workspace|
      slug = '2026-09-22-pending-direct-team'
      calls = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:select_tmux_for_slug!) { |_selected| nil }
        define_method(:reject_conflicting_lifecycle_journal!) { |_selected| nil }
        define_method(:invoke_team_command) do |_selected, **_options|
          calls << :mutated
          'ok'
        end
      end
      runner = runner_class.new(
        workspace:, tmux: InertTmux.new, out: StringIO.new,
        err: StringIO.new, today: Date.new(2026, 9, 22),
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
        cwd: workspace, portal_command: ['workspace-portal-test']
      )
      runner.ensure_tracking_files(slug)
      workspace_id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      receipt_path = File.join(
        runner.instance_variable_get(:@workspace_state_root), 'portal',
        workspace_id, 'creations', "#{slug}.json"
      )
      FileUtils.mkdir_p(File.dirname(receipt_path))
      receipt = { 'schema' => 3, 'workspace' => workspace,
                  'request' => { 'slug' => slug }, 'state' => 'running' }
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)

      error = assert_raises(DevSession::Error) do
        runner.team(slug, action: 'add', role: 'architect', model: 'gpt-6-sol', effort: 'xhigh')
      end
      assert_match(/wait for session creation/, error.message)
      assert_empty(calls)

      receipt['state'] = 'ready'
      File.write(receipt_path, JSON.generate(receipt))
      assert_equal('ok', runner.team(slug, action: 'add', role: 'architect', model: 'gpt-6-sol', effort: 'xhigh'))
      assert_equal([:mutated], calls)
    end
  end

  private

  def managed_runner_for(workspace)
    DevSession::Runner.new(
      workspace:,
      tmux: InertTmux.new,
      out: StringIO.new,
      err: StringIO.new,
      today: Date.new(2026, 9, 22),
      env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
      cwd: workspace
    )
  end

  def direct_creation_runner(workspace, scenario)
    DirectCreationRunner.new(
      workspace:,
      authority_dir: File.join(workspace, 'runtime-authority'),
      tmux: InertTmux.new,
      out: StringIO.new,
      err: StringIO.new,
      today: Date.new(2026, 9, 22),
      env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
      cwd: workspace,
      portal_command: ['workspace-portal-test'],
      command_runner: DirectCreationCommandRunner.new(scenario),
      scenario:
    )
  end

  def start_direct_creation(runner, slug, goal, preset_path)
    runner.start(
      slug,
      as_is: true,
      new: false,
      attach: false,
      run_codex: true,
      goal_file: goal,
      json: true,
      team_preset: preset_path,
      model: 'model-lead',
      effort: 'high'
    )
  end

  def managed_creation_journal(slug, state:)
    journal = {
      'schema' => 2, 'slug' => slug, 'goal_sha256' => 'a' * 64,
      'run_codex' => true, 'state' => state, 'model' => 'model-lead',
      'effort' => 'high', 'agent_team_binding' => "v1.eyJ4IjoieSJ9.#{'b' * 64}",
      'agent_team_binding_digest' => 'b' * 64
    }
    journal['tmux_identity'] = 'c' * 64 if state == 'creating'
    journal
  end

  def direct_team_preset
    {
      'id' => 'delegated', 'name' => 'Full team',
      'description' => 'Lead, architect, and implementer threads.',
      'catalogDigest' => 'a' * 64, 'teamDigest' => 'b' * 64,
      'leadModel' => 'model-lead', 'leadEffort' => 'high',
      'roles' => %w[lead architect0 implementer0],
      'members' => [
        {
          'role' => 'architect', 'address' => 'architect0',
          'behavior' => 'designer', 'access' => 'read_only',
          'model' => 'model-architect', 'reasoningEffort' => 'medium'
        },
        {
          'role' => 'implementer', 'address' => 'implementer0',
          'behavior' => 'implementer', 'access' => 'workspace_write',
          'model' => 'model-implementer', 'reasoningEffort' => 'xhigh'
        }
      ]
    }
  end
end
