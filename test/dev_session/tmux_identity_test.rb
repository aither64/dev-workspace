# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_tmux_targets_do_not_prefix_match_a_longer_session
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'
    longer_slug = "#{slug}-long"

    with_workspace do |workspace|
      tmux_run(socket, 'new-session', '-d', '-s', longer_slug, '-c', workspace)
      command_runner = DevSession::CommandRunner.new(
        out: StringIO.new,
        err: StringIO.new
      )
      tmux = DevSession::Tmux.new(runner: command_runner, socket:)

      refute(tmux.session(slug))
      assert(tmux.session(longer_slug))

      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/session not found/, error.message)
      assert(tmux_session_exists?(socket, longer_slug))

      removed_slug = '2026-06-06-removed'
      tmux_run(socket, 'new-session', '-d', '-s', removed_slug, '-c', workspace)
      removed = tmux.session(removed_slug)
      tmux_run(socket, 'kill-session', '-t', "#{removed.id}:")
      assert_nil(tmux.session_by_id(removed.id))
      assert(tmux_session_exists?(socket, longer_slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_tmux_identity_uses_the_connected_socket_after_its_directory_is_renamed
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    Dir.mktmpdir('dev-session-renamed-socket-test') do |directory|
      old_directory = File.join(directory, 'old')
      new_directory = File.join(directory, 'new')
      FileUtils.mkdir_p(old_directory)
      old_socket = File.join(old_directory, 'tmux.sock')
      new_socket = File.join(new_directory, 'tmux.sock')
      slug = '2026-06-06-demo'
      command_runner = DevSession::CommandRunner.new(out: StringIO.new, err: StringIO.new)

      assert(system('tmux', '-S', old_socket, 'new-session', '-d', '-s', slug))
      File.rename(old_directory, new_directory)
      tmux = DevSession::Tmux.new(runner: command_runner, socket: new_socket)

      assert_equal(old_socket, tmux.capture('display-message', '-p', '#{socket_path}').first.strip)
      assert_equal(new_socket, tmux.session(slug).socket_path)
    ensure
      system('tmux', '-S', new_socket, 'kill-server', out: File::NULL, err: File::NULL) if new_socket
      File.unlink(new_socket) if new_socket && File.socket?(new_socket)
    end
  end

  def test_tmux_conditional_retirement_kills_only_the_matching_identity
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'
    command_runner = DevSession::CommandRunner.new(
      out: StringIO.new,
      err: StringIO.new
    )
    tmux = DevSession::Tmux.new(runner: command_runner, socket:)

    tmux_run(socket, 'new-session', '-d', '-s', slug)
    session = tmux.session(slug)
    token = 'a' * 64
    tmux_run(
      socket, 'set-environment', '-t', "#{session.id}:",
      DevSession::ENV_TMUX_IDENTITY, token
    )

    refute(tmux.kill_session_if_identity(session.id, 'b' * 64))
    assert(tmux_session_exists?(socket, slug))
    assert(tmux.kill_session_if_identity(session.id, token))
    refute(tmux_session_exists?(socket, slug))
  ensure
    tmux_run(socket, 'kill-server', allow_failure: true) if socket
  end

  def test_tmux_lookup_treats_a_successful_blank_response_as_absent
    status = Object.new
    status.define_singleton_method(:success?) { true }
    command_runner = Object.new
    command_runner.define_singleton_method(:capture) do |_argv, allow_failure:|
      raise 'tmux lookup must allow a missing target' unless allow_failure

      [" \n", '', status]
    end
    tmux = DevSession::Tmux.new(runner: command_runner, socket: '/run/test.sock')

    assert_nil(tmux.session('2026-06-06-missing'))
    assert_nil(tmux.session_by_id('$11'))
  end

  def test_tmux_lookup_treats_the_missing_target_socket_record_as_absent
    status = Object.new
    status.define_singleton_method(:success?) { true }
    phantom = Array.new(12, '')
    phantom[6] = '/run/test.sock'
    command_runner = Object.new
    command_runner.define_singleton_method(:capture) do |_argv, allow_failure:|
      raise 'tmux lookup must allow a missing target' unless allow_failure

      [phantom.join("\t") + "\n", '', status]
    end
    tmux = DevSession::Tmux.new(runner: command_runner, socket: '/run/test.sock')

    assert_nil(tmux.session('2026-06-06-missing'))
    assert_nil(tmux.session_by_id('$11'))
  end

  def test_tmux_lookup_ignores_global_environment_on_a_missing_target
    status = Object.new
    status.define_singleton_method(:success?) { true }
    phantom = [
      '', '', '1', 'global-slug', '/global-workspace', 'global-slug',
      '/run/test.sock', 'global-thread', '/run/global-codex.sock', '0.153.4',
      '%42', 'a' * 64
    ]
    command_runner = Object.new
    command_runner.define_singleton_method(:capture) do |_argv, allow_failure:|
      raise 'tmux lookup must allow a missing target' unless allow_failure

      [phantom.join("\t") + "\n", '', status]
    end
    tmux = DevSession::Tmux.new(runner: command_runner, socket: '/run/test.sock')

    assert_nil(tmux.session('2026-06-06-missing'))
    assert_nil(tmux.session_by_id('$11'))
  end

  def test_tmux_lookup_rejects_partial_or_truncated_missing_target_records
    status = Object.new
    status.define_singleton_method(:success?) { true }
    responses = [
      ['$11', nil, nil, 'unexpected-slug', nil, nil, '/run/test.sock', nil, nil, nil, nil, nil],
      Array.new(11, '').tap { |fields| fields[5] = '/workspace' }
    ]

    responses.each do |fields|
      command_runner = Object.new
      command_runner.define_singleton_method(:capture) do |_argv, allow_failure:|
        raise 'tmux lookup must allow a missing target' unless allow_failure

        [fields.map(&:to_s).join("\t") + "\n", '', status]
      end
      tmux = DevSession::Tmux.new(runner: command_runner, socket: '/run/test.sock')

      error = assert_raises(DevSession::Error) { tmux.session_by_id('$11') }
      assert_includes(error.message, 'invalid session identity')
    end
  end

  def test_tmux_id_lookup_rejects_a_mismatched_identity
    status = Object.new
    status.define_singleton_method(:success?) { true }
    identity = [
      '$12', '2026-06-06-other', '1', '2026-06-06-other', '/workspace',
      '2026-06-06-other', '/run/test.sock', '', '', '', '', 'a' * 64
    ].join("\t") + "\n"
    command_runner = Object.new
    command_runner.define_singleton_method(:capture) do |_argv, allow_failure:|
      raise 'tmux lookup must allow a missing target' unless allow_failure

      [identity, '', status]
    end
    tmux = DevSession::Tmux.new(runner: command_runner, socket: '/run/test.sock')

    error = assert_raises(DevSession::Error) { tmux.session_by_id('$11') }
    assert_includes(error.message, 'session "$12" for exact target $11')
    assert_nil(tmux.session('2026-06-06-missing'))
  end

  def test_runtime_retirement_keeps_authority_when_tmux_returns_the_wrong_id
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      tmux = MismatchedIdentityAfterKillTmux.new(
        slug,
        workspace:,
        socket_path: '/run/test.sock',
        id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      session = tmux.session(slug)
      runner.send(:write_session_authority, slug, session, state: 'ready')
      runner.send(:select_tmux_for_slug!, slug)

      error = assert_raises(DevSession::Error) do
        runner.send(:retire_session_runtime!, slug, session)
      end

      assert_includes(error.message, 'session "$12" while verifying removal of $11')
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_runtime_retirement_does_not_kill_a_same_name_replacement
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      tmux = ReplacedTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:, authority_dir:)
      stale = DevSession::Tmux::Session.new(
        id: '$11',
        name: slug,
        mark: '1',
        slug:,
        workspace:,
        environment_slug: slug,
        socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, stale, state: 'ready')
      runner.send(:select_tmux_for_slug!, slug)

      error = assert_raises(DevSession::Error) do
        runner.send(:retire_session_runtime!, slug, tmux.session(slug))
      end

      assert_includes(error.message, 'does not match trusted authority')
      refute(tmux.kill_attempted)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_runtime_retirement_rejects_a_reused_id_with_a_new_identity
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      original = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: 'a' * 64
      )
      runner = runner_for(workspace, tmux: original, authority_dir:)
      runner.send(:write_session_authority, slug, original.session(slug), state: 'ready')

      replacement = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: 'b' * 64
      )
      runner.instance_variable_set(:@tmux, replacement)
      runner.instance_variable_set(:@default_tmux, replacement)
      runner.send(:select_tmux_for_slug!, slug)

      error = assert_raises(DevSession::Error) do
        runner.send(:retire_session_runtime!, slug, replacement.session(slug))
      end

      assert_includes(error.message, 'does not match trusted authority')
      refute(replacement.killed)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_runtime_retirement_without_authority_uses_the_creation_identity
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ReplacedDuringConditionalKillTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.send(:retire_session_runtime!, slug, tmux.session(slug))
      end

      assert_includes(error.message, 'changed during removal')
      assert(tmux.conditional_kill_attempted)
      refute(tmux.killed)
    end
  end

  def test_runtime_retirement_without_authority_rejects_a_tokenless_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ManagedTmux.new(slug, workspace:, identity_token: nil)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.send(:retire_session_runtime!, slug, tmux.session(slug))
      end

      assert_includes(error.message, 'lacks a trusted creation identity')
      refute(tmux.killed)
    end
  end

  def test_runtime_retirement_refuses_a_live_legacy_authority
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      authority_path = File.join(authority_dir, "#{slug}.json")
      authority = JSON.parse(File.read(authority_path))
      authority.delete('tmux_identity')
      File.write(authority_path, JSON.generate(authority) + "\n")
      runner.send(:select_tmux_for_slug!, slug)

      error = assert_raises(DevSession::Error) do
        runner.send(:retire_session_runtime!, slug, tmux.session(slug))
      end

      assert_includes(error.message, 'lacks a tmux creation identity')
      refute(tmux.killed)
      assert(File.file?(authority_path))
    end
  end

  def test_runtime_retirement_does_not_kill_after_authority_was_deleted
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      replacement = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$12'
      )
      runner = runner_for(workspace, tmux: replacement, authority_dir:)

      error = assert_raises(DevSession::Error) do
        runner.send(:retire_session_runtime!, slug, replacement.session(slug))
      end

      assert_includes(error.message, 'untrusted same-name tmux session')
      refute(replacement.killed)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_start_does_not_adopt_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ReplacedDuringCreateTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end

      assert_match(/changed during creation/, error.message)
      assert_includes(tmux.new_session_args, '-P')
      assert_includes(tmux.new_session_args, '#{session_id}')
      assert_equal(2, tmux.name_lookups)
      assert_empty(tmux.mutations)
    end
  end

  def test_start_keeps_the_created_identity_through_sync
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ReplacedBeforeSyncTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end

      assert_match(/session changed during operation/, error.message)
      assert_equal(2, tmux.name_lookups)
      refute(tmux.mutations.flatten.include?('$replacement:'))
    end
  end

  def test_start_prints_an_identity_bound_attach_command
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      tmux = ManagedTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:, out:)

      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      expected = ['tmux', 'attach-session', '-t', '$managed:']
                 .map(&:shellescape)
                 .join(' ')
      assert_includes(out.string, "attach: #{expected}")
      refute_includes(out.string, "=#{slug}:")
    end
  end

  def test_start_preserves_a_parsed_legacy_authority_without_an_empty_identity
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: ''
      )
      runner = runner_for(workspace, tmux:, authority_dir:)

      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      authority = JSON.parse(
        File.read(File.join(authority_dir, "#{slug}.json"))
      )
      refute(authority.key?('tmux_identity'))
      refute(tmux.killed)
    end
  end

  def test_start_normalizes_a_legacy_symlinked_workspace_identity
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      Dir.mktmpdir('dev-session-workspace-alias') do |directory|
        alias_path = File.join(directory, 'workspace')
        FileUtils.ln_s(workspace, alias_path)
        out = StringIO.new
        tmux = LegacyWorkspaceTmux.new(slug, workspace: alias_path)
        runner = runner_for(workspace, tmux:, out:)

        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

        assert_equal(workspace, tmux.workspace)
        assert_includes(out.string, '$managed:')
      end
    end
  end

  def test_start_rolls_back_a_partial_tmux_layout_and_retries_creation
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = PartialCreateTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      2.times do
        error = assert_raises(DevSession::Error) do
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
        end
        assert_match(/split failed/, error.message)
      end

      assert_equal(2, tmux.split_attempts)
      assert_equal(2, tmux.kill_count)
    end
  end

  def test_creation_recovery_kills_only_the_exact_unmarked_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = RecordingTmux.new
      runner = runner_for(workspace, tmux:)
      session = DevSession::Tmux::Session.new(
        id: '$partial',
        name: slug,
        mark: '',
        slug: '',
        workspace:,
        environment_slug: slug,
        identity_token: 'a' * 64
      )

      runner.send(:reconcile_creation_tmux_session!, slug, session, 'a' * 64)

      assert_equal(
        [['conditional-kill-session', '$partial', 'a' * 64]],
        tmux.mutations
      )

      replacement = session.dup
      replacement.identity_token = 'b' * 64
      error = assert_raises(DevSession::Error) do
        runner.send(:reconcile_creation_tmux_session!, slug, replacement, 'a' * 64)
      end
      assert_includes(error.message, 'not recoverable')
      assert_equal(1, tmux.mutations.length)
    end
  end

  def test_creation_retry_upgrades_a_legacy_journal_only_attempt
    with_workspace do |workspace|
      slug = '2026-06-06-legacy-creation'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume safely.\n")
      runner = runner_for(workspace)
      journal = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      journal.delete('tmux_identity')
      runner.send(
        :write_creation_journal,
        runner.send(:creation_journal_file, slug),
        journal,
        create: false
      )

      upgraded = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )

      assert_match(/\A[0-9a-f]{64}\z/, upgraded.fetch('tmux_identity'))
    end
  end

  def test_creation_retry_refuses_a_live_legacy_unbound_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-legacy-live-creation'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume safely.\n")
      setup = runner_for(workspace)
      journal = setup.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      journal.delete('tmux_identity')
      setup.send(
        :write_creation_journal,
        setup.send(:creation_journal_file, slug),
        journal,
        create: false
      )
      tmux = ManagedTmux.new(slug, workspace:, identity_token: nil)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.send(
          :prepare_creation_journal,
          slug,
          goal,
          exclusive: true,
          run_codex: true,
          model: nil,
          effort: nil
        )
      end

      assert_includes(error.message, 'live tmux session without a trusted identity')
      refute(tmux.killed)
    end
  end

  def test_ready_session_restart_recovers_only_its_journal_bound_partial_tmux
    with_workspace do |workspace|
      slug = '2026-06-06-restart'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(slug)
      setup.send(:ensure_portal_manifest, slug)
      journal = setup.send(:prepare_start_journal!, slug)
      tmux = PartialManagedTmux.new(
        slug, workspace:, identity_token: journal.fetch('tmux_identity')
      )
      created = DevSession::Tmux::Session.new(
        id: '$12', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, identity_token: journal.fetch('tmux_identity')
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| created }
        define_method(:sync_slug) { |*_arguments, **_keywords| created }
      end
      runner = runner_class.new(
        workspace:, tmux:, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      assert(tmux.killed)
      refute(File.exist?(setup.send(:start_journal_file, slug)))
    end
  end

  def test_ready_session_restart_refuses_a_partial_tmux_with_another_identity
    with_workspace do |workspace|
      slug = '2026-06-06-restart'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(slug)
      setup.send(:ensure_portal_manifest, slug)
      setup.send(:prepare_start_journal!, slug)
      tmux = PartialManagedTmux.new(slug, workspace:, identity_token: 'b' * 64)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end

      assert_includes(error.message, 'does not match start identity')
      refute(tmux.killed)
      assert(File.exist?(setup.send(:start_journal_file, slug)))
    end
  end

end
