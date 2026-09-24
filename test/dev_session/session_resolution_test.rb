# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_slug_validation_rejects_paths_and_tmux_targets
    with_workspace do |workspace|
      runner = runner_for(workspace)

      assert_raises(DevSession::Error) do
        runner.build_slug('../escape', as_is: false)
      end

      assert_raises(DevSession::Error) do
        runner.build_slug('demo:1', as_is: false)
      end
    end
  end

  def test_lookup_slug_reports_ambiguity
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-demo'))
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', '2026-06-06-demo'))

      runner = runner_for(workspace)
      error = assert_raises(DevSession::Error) do
        runner.lookup_slug('demo', as_is: false)
      end

      assert_match(/ambiguous/, error.message)
      assert_match(/2026-06-05-demo/, error.message)
      assert_match(/2026-06-06-demo/, error.message)
    end
  end

  def test_lookup_slug_ignores_an_unsafe_legacy_managed_session_name
    with_workspace do |workspace|
      tmux = ManagedTmux.new('x/2026-06-06-demo', workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.lookup_slug('demo', as_is: false)
      end

      assert_match(/no slug found/, error.message)
    end
  end

  def test_start_slug_resolution_reuses_existing_unique_match
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-service-health-checks'))

      runner = runner_for(workspace)

      assert_equal(
        '2026-06-05-service-health-checks',
        runner.resolve_start_slug('service-health-checks', as_is: false, new: false)
      )
    end
  end

  def test_start_slug_resolution_creates_today_slug_when_no_match_exists
    with_workspace do |workspace|
      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-demo',
        runner.resolve_start_slug('demo', as_is: false, new: false)
      )
    end
  end

  def test_start_slug_resolution_reports_ambiguity
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-demo'))
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-06-demo'))

      runner = runner_for(workspace)
      error = assert_raises(DevSession::Error) do
        runner.resolve_start_slug('demo', as_is: false, new: false)
      end

      assert_match(/ambiguous/, error.message)
    end
  end

  def test_start_new_uses_today_slug_despite_existing_matches
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-04-demo'))
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-demo'))

      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-demo',
        runner.resolve_start_slug('demo', as_is: false, new: true)
      )
    end
  end

  def test_start_new_reuses_today_slug_when_it_already_exists
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-06-demo'))

      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-demo',
        runner.resolve_start_slug('demo', as_is: false, new: true)
      )
    end
  end

  def test_start_new_and_as_is_are_mutually_exclusive
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(DevSession::Error) do
        runner.resolve_start_slug('demo', as_is: true, new: true)
      end

      assert_match(/cannot be used together/, error.message)
    end
  end

  def test_start_new_rejects_dated_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(DevSession::Error) do
        runner.resolve_start_slug('2026-06-05-demo', as_is: false, new: true)
      end

      assert_match(/requires a short name/, error.message)
    end
  end

  def test_start_as_is_rejects_an_unsafe_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(DevSession::Error) do
        runner.resolve_start_slug('../escape', as_is: true, new: false)
      end

      assert_match(/invalid slug/, error.message)
    end
  end

  def test_start_rejects_json_attach_before_creating_session_state
    with_workspace do |workspace|
      slug = '2026-06-06-demo'

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).start(
          slug,
          as_is: true,
          new: false,
          attach: true,
          run_codex: false,
          json: true
        )
      end

      assert_match(/--json cannot be combined with --attach/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_tracking_files_are_created_once
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'

      runner.ensure_tracking_files(slug)
      plan = File.join(workspace, 'work', slug, 'plan.md')
      state = File.join(workspace, 'work', slug, 'state.md')

      File.write(plan, "custom plan\n")
      runner.ensure_tracking_files(slug)

      assert_equal("custom plan\n", File.read(plan))
      assert_includes(File.read(state), '## Commands run')
      assert_match(/\A---\nlifecycle: active\n---\n/, File.read(state))
      assert(File.directory?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_start_creates_a_portal_manifest_and_prints_the_stable_url
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      socket_path = '/run/user/1000/tmux-1000/default'
      tmux = ManagedTmux.new(slug, workspace:, socket_path:)
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_url: 'https://workspace.example.test/'
      )

      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal(1, manifest['schema'])
      assert_equal(slug, manifest['slug'])
      refute(manifest.key?('tmux'))
      assert_includes(out.string, "portal: https://workspace.example.test/#{slug}/")
    end
  end

  def test_tmux_socket_can_come_from_deployment_environment
    with_workspace do |workspace|
      socket = '/run/dev-workspace-tmux/tmux.sock'
      runner = DevSession::Runner.new(
        workspace:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'DEV_SESSION_TMUX_SOCKET' => socket }
      )

      assert_equal(socket, runner.instance_variable_get(:@tmux).socket)
    end
  end

  def test_portal_runtime_mode_fails_closed_and_propagates_to_sessions
    with_workspace do |workspace|
      error = assert_raises(DevSession::Error) do
        DevSession::Runner.new(
          workspace:,
          tmux: NullTmux.new,
          require_runtime: true,
          out: StringIO.new,
          err: StringIO.new,
          env: {}
        )
      end
      assert_includes(error.message, 'portal runtime configuration is incomplete')

      error = assert_raises(DevSession::Error) do
        DevSession::Runner.new(
          workspace:,
          tmux: NullTmux.new,
          authority_dir: '/run/workspace-authority',
          tmux_socket: '/run/workspace-tmux/tmux.sock',
          codex_socket: '/run/workspace-codex/app-server.sock',
          codex_version: '0.152.1',
          codex_command: '/bin/true',
          portal_command: ['/run/current-system/sw/bin/workspace-portal'],
          cluster_providers: {
            'alpha' => File.join(workspace, 'missing-devcluster'),
            'beta' => RbConfig.ruby
          },
          require_runtime: true,
          out: StringIO.new,
          err: StringIO.new,
          env: {}
        )
      end
      assert_equal('portal runtime commands are unavailable: alpha-devcluster', error.message)

      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        authority_dir: '/run/workspace-authority',
        tmux_socket: '/run/workspace-tmux/tmux.sock',
        codex_socket: '/run/workspace-codex/app-server.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        portal_command: ['/run/current-system/sw/bin/workspace-portal'],
        cluster_providers: { 'alpha' => RbConfig.ruby, 'beta' => RbConfig.ruby },
        require_runtime: true,
        out: StringIO.new,
        err: StringIO.new,
        env: {}
      )
      environment = runner.send(:session_environment, '2026-06-06-demo')
      contract = JSON.parse(
        File.read(File.expand_path('../../portal/internal/session/runtime-contract.json', __dir__))
      )
      assert_equal(
        DevSession::MAX_MESSAGE_BYTES,
        contract.fetch('maxMessageBytes')
      )
      assert_equal(
        DevSession::TRACKING_MAX_SIZE,
        contract.fetch('trackingMaxBytes')
      )
      assert_equal(
        DevSession::AGENT_TEAM_PERSISTED_SCALAR_MAX_BYTES,
        contract.fetch('agentTeamRegistration').fetch('maxPersistedScalarBytes')
      )
      assert_equal(
        DevSession::LIFECYCLE_JOURNALS,
        contract.fetch('lifecycleJournals')
      )
      expected_journals = contract.fetch('lifecycleJournals').to_h do |journal|
        [journal.fetch('command'), journal.fetch('name')]
      end
      assert_equal(expected_journals, DevSession::LIFECYCLE_JOURNAL_NAMES)
      assert_equal(%w[archive delete revive], expected_journals.keys.sort)
      assert_equal(expected_journals.length, expected_journals.values.uniq.length)
      expected_journals.each do |command, name|
        assert_equal(
          File.join(workspace, 'worktrees', '.locks', "2026-06-06-demo.#{name}.json"),
          runner.send(:lifecycle_journal_file, '2026-06-06-demo', command)
        )
      end
      environment_keys = contract.fetch('threadEnvironmentKeys')
      assert_equal(environment_keys.sort, environment.keys.sort)
      assert_equal(
        (environment_keys - [DevSession::ENV_REQUIRE_RUNTIME]).sort,
        DevSession::THREAD_ENV_ARGUMENTS.keys.sort
      )
      assert_equal('1', environment.fetch(DevSession::ENV_REQUIRE_RUNTIME))
      assert_equal(
        DevSession::DEFAULT_PORTAL_BASE_URL,
        environment.fetch(DevSession::ENV_PORTAL_BASE_URL)
      )
      assert_equal(
        "#{DevSession::DEFAULT_PORTAL_BASE_URL}/2026-06-06-demo/",
        environment.fetch(DevSession::ENV_PORTAL_URL)
      )
      assert_equal(
        '/run/current-system/sw/bin/workspace-portal',
        environment.fetch(DevSession::ENV_PORTAL_COMMAND)
      )
    end
  end

  def test_deployment_runtime_flags_are_accepted
    with_workspace do |workspace|
      contract = JSON.parse(
        File.read(File.expand_path('../../portal/internal/session/runtime-contract.json', __dir__))
      )
      values = {
        '--workspace' => workspace,
        '--host-profile' => File.join(workspace, 'profile'),
        '--expected-host-generation' => File.join(workspace, 'generation'),
        '--authority-dir' => File.join(workspace, 'authority'),
        '--tmux-socket' => File.join(workspace, 'tmux.sock'),
        '--codex-command' => '/bin/true',
        '--codex-socket' => File.join(workspace, 'codex.sock'),
        '--codex-version' => 'test-version',
        '--portal-command' => '/bin/true',
        '--portal-base-url' => 'https://workspace.example.test',
        '--cluster-provider' => "alpha=#{RbConfig.ruby}",
        '--transition-lock' => File.join(workspace, 'transition.lock')
      }
      FileUtils.mkdir_p(values.fetch('--expected-host-generation'))
      File.write(values.fetch('--transition-lock'), '')
      File.symlink(
        values.fetch('--expected-host-generation'), values.fetch('--host-profile')
      )
      values['--expected-host-profile-token'] = profile_link_token(
        values.fetch('--host-profile')
      )
      arguments = contract.fetch('devSessionFlags').flat_map do |option|
        option == '--require-runtime' ? [option] : [option, values.fetch(option)]
      end
      out = StringIO.new
      err = StringIO.new
      status = DevSession::CLI.new(arguments + ['validate'], out:, err:).run
      assert_equal(0, status, err.string)
    end
  end

  def test_global_option_separator_seals_the_deployment_runtime
    with_workspace do |workspace|
      fixed_workspace = File.join(workspace, 'fixed')
      FileUtils.mkdir_p(fixed_workspace)
      arguments = [
        '--require-runtime',
        '--workspace', fixed_workspace,
        '--authority-dir', File.join(workspace, 'authority'),
        '--tmux-socket', File.join(workspace, 'tmux.sock'),
        '--codex-command', '/bin/true',
        '--codex-socket', File.join(workspace, 'codex.sock'),
        '--codex-version', 'test-version',
        '--portal-command', '/bin/true',
        '--portal-base-url', 'https://workspace.example.test',
        '--'
      ]

      out = StringIO.new
      err = StringIO.new
      status = DevSession::CLI.new(arguments + ['--help'], out:, err:).run
      assert_equal(0, status, err.string)
      assert_includes(out.string, 'Usage:')

      %w[--workspace --workspace=/tmp/caller].each do |override|
        caller_arguments = override == '--workspace' ? [override, '/tmp/caller'] : [override]
        out = StringIO.new
        err = StringIO.new
        status = DevSession::CLI.new(
          arguments + caller_arguments + ['validate'], out:, err:
        ).run
        assert_equal(1, status)
        assert_includes(err.string, "unknown command: #{override}")
      end
    end
  end

  def test_resolved_session_url_is_not_reused_as_the_portal_base
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      out = StringIO.new
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        env: {
          DevSession::ENV_PORTAL_BASE_URL => 'https://workspace.example.test',
          DevSession::ENV_PORTAL_URL => "https://workspace.example.test/#{slug}/"
        }
      )

      assert_equal(
        "https://workspace.example.test/#{slug}/",
        runner.url(slug, as_is: true)
      )
      assert_equal("https://workspace.example.test/#{slug}/\n", out.string)
    end
  end

  def test_absolute_tmux_socket_ignores_tmux_tmpdir_and_is_persisted
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      Dir.mktmpdir('dev-session-socket') do |socket_directory|
        socket = File.join(socket_directory, 'tmux.sock')
        authority_dir = File.join(socket_directory, 'authority')
        former_tmpdir = ENV['TMUX_TMPDIR']
        ENV['TMUX_TMPDIR'] = File.join(socket_directory, 'ignored')
        begin
          runner = DevSession::Runner.new(
            workspace:,
            tmux_socket: socket,
            authority_dir:,
            out: StringIO.new,
            err: StringIO.new,
            today: TODAY,
            env: {}
          )
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

          authority_lock = File.join(authority_dir, "#{slug}.lock")
          authority = JSON.parse(File.read(File.join(authority_dir, "#{slug}.json")))
          assert_match(/\A[0-9a-f]{64}\z/, authority.fetch('tmux_identity'))
          assert(File.file?(authority_lock))
          assert_equal(0o600, File.stat(authority_lock).mode & 0o777)
          _stdout, _stderr, status = Open3.capture3(
            { 'TMUX_TMPDIR' => File.join(socket_directory, 'elsewhere') },
            'tmux', '-S', socket, 'has-session', '-t', "=#{slug}:"
          )
          assert(status.success?, 'explicit tmux socket did not survive TMUX_TMPDIR drift')

          out = StringIO.new
          ordinary_runner = DevSession::Runner.new(
            workspace:,
            authority_dir:,
            out:,
            err: StringIO.new,
            today: TODAY,
            env: {}
          )
          ordinary_runner.list(slug, as_is: true)
          assert_includes(out.string, 'managed')

          mismatch = DevSession::Runner.new(
            workspace:,
            tmux_socket: File.join(socket_directory, 'other.sock'),
            authority_dir:,
            out: StringIO.new,
            err: StringIO.new,
            today: TODAY,
            env: {}
          )
          error = assert_raises(DevSession::Error) do
            mismatch.list(slug, as_is: true)
          end
          assert_includes(error.message, 'does not match trusted session authority')

          ordinary_runner.stop(slug, as_is: true)
          refute(tmux_session_exists?(socket, slug))
          refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
        ensure
          Open3.capture3('tmux', '-S', socket, 'kill-server')
          former_tmpdir.nil? ? ENV.delete('TMUX_TMPDIR') : ENV['TMUX_TMPDIR'] = former_tmpdir
        end
      end
    end
  end

  def test_cross_server_attach_unsets_tmux_instead_of_switching_the_other_server
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      target = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/dev-workspace-tmux/tmux.sock'
      )
      calls = []
      runner = DevSession::Runner.new(
        workspace:,
        tmux: target,
        process_exec: ->(environment, argv) { calls << [environment, argv] },
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {
          'TMUX' => '/tmp/tmux-1000/default,123,0',
          'TMUX_PANE' => '%1'
        }
      )
      runner.ensure_tracking_files(slug)

      runner.attach(slug, as_is: true)

      assert_equal(1, calls.length)
      assert_equal({'TMUX' => nil, 'TMUX_PANE' => nil}, calls[0][0])
      assert_equal(
        ['tmux', 'attach-session', '-t', '$managed:'],
        calls[0][1]
      )
    end
  end

  def test_exact_slug_attach_from_a_normal_shell_uses_the_target_tmux_server
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      target = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/dev-workspace-tmux/tmux.sock'
      )
      calls = []
      runner = DevSession::Runner.new(
        workspace:,
        tmux: target,
        process_exec: ->(environment, argv) { calls << [environment, argv] },
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.ensure_tracking_files(slug)

      runner.attach(slug, as_is: false)

      assert_equal(1, calls.length)
      assert_equal({}, calls[0][0])
      assert_equal(
        ['tmux', 'attach-session', '-t', '$managed:'],
        calls[0][1]
      )
    end
  end

end
