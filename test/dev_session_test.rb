# frozen_string_literal: true

require 'date'
require 'fileutils'
require 'json'
require 'minitest/autorun'
require 'open3'
require 'rbconfig'
require 'shellwords'
require 'stringio'
require 'tmpdir'

load File.expand_path('../libexec/dev-session', __dir__)

class DevSessionTest < Minitest::Test
  class NullTmux
    def argv(*args)
      ['tmux', *args]
    end

    def session(_slug)
      nil
    end

    def session_by_id(_id)
      nil
    end

    def managed_session_identities
      []
    end

    def current_session_identity(_pane)
      nil
    end

    def pane_session_id(_pane)
      nil
    end

    def pane_current_command(_pane)
      nil
    end
  end

  class CurrentTmux < NullTmux
    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
    end

    def current_session_identity(_pane)
      VpsfreeDevSession::Tmux::Session.new(
        id: '$current',
        name: @slug,
        mark: '1',
        slug: @slug,
        workspace: @workspace
      )
    end
  end

  class ManagedTmux < NullTmux
    attr_reader :killed, :quiesced, :sent_commands

    def initialize(
      slug,
      workspace:,
      on_kill: nil,
      socket_path: nil,
      codex_thread_id: nil,
      codex_socket_path: nil,
      codex_client_version: nil,
      codex_pane_id: nil,
      pane_current_command: nil,
      id: '$managed'
    )
      @slug = slug
      @workspace = workspace
      @id = id
      @on_kill = on_kill
      @socket_path = socket_path
      @codex_thread_id = codex_thread_id
      @codex_socket_path = codex_socket_path
      @codex_client_version = codex_client_version
      @codex_pane_id = codex_pane_id || (codex_thread_id && '%1')
      @pane_current_command = pane_current_command || (codex_thread_id && 'codex')
      @killed = false
      @quiesced = false
      @sent_commands = []
    end

    def session(slug)
      return unless slug == @slug && !@killed

      identity
    end

    def session_by_id(id)
      return unless id == @id && !@killed

      identity
    end

    def managed_session_identities
      @killed ? [] : [identity]
    end

    def windows(_session)
      []
    end

    def pane_session_id(pane)
      pane == @codex_pane_id && !@killed ? @id : nil
    end

    def pane_current_command(pane)
      pane == @codex_pane_id && !@killed ? @pane_current_command : nil
    end

    def argv(*args)
      ['tmux', *args]
    end

    def run(*args)
      if args.first == 'respawn-pane'
        @quiesced = true
        @pane_current_command = File.basename(args.last)
        @on_kill&.call
        return
      end
      if args.first == 'send-keys'
        @quiesced = false
        if args.include?('-l')
          @sent_commands << args.last
          @pane_current_command = 'codex'
        end
      end
      if args.first == 'set-option' && args[-2] == VpsfreeDevSession::SESSION_CODEX_VERSION
        @codex_client_version = args.last
        return
      end
      targets = ["#{@id}:", @slug]
      return unless args.first(2) == ['kill-session', '-t'] && targets.include?(args[2])

      @on_kill&.call
      @killed = true
    end

    private

    def identity
      VpsfreeDevSession::Tmux::Session.new(
        id: @id,
        name: @slug,
        mark: '1',
        slug: @slug,
        workspace: @workspace,
        environment_slug: @slug,
        socket_path: @socket_path,
        codex_thread_id: @codex_thread_id,
        codex_socket_path: @codex_socket_path,
        codex_client_version: @codex_client_version,
        codex_pane_id: @codex_pane_id
      )
    end
  end

  class LegacyWorkspaceTmux < ManagedTmux
    attr_reader :workspace

    def initialize(slug, workspace:)
      super
      @workspace = workspace
    end

    def run(*args)
      if args.first == 'set-environment' &&
         args[-2] == VpsfreeDevSession::ENV_WORKSPACE
        @workspace = args.last
      else
        super
      end
    end
  end

  class UnmanagedTmux < NullTmux
    def initialize(slug)
      @slug = slug
    end

    def session(slug)
      return unless slug == @slug

      VpsfreeDevSession::Tmux::Session.new(
        id: '$unmanaged',
        name: @slug,
        mark: '',
        slug: '',
        workspace: ''
      )
    end


  end

  class ReplacedTmux < NullTmux
    attr_reader :kill_attempted

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @kill_attempted = false
    end

    def session(slug)
      return unless slug == @slug

      VpsfreeDevSession::Tmux::Session.new(
        id: '$original',
        name: @slug,
        mark: '1',
        slug: @slug,
        workspace: @workspace
      )
    end

    def session_by_id(_id)
      nil
    end

    def run(*_args)
      @kill_attempted = true
    end
  end

  class ReplacedDuringCreateTmux < NullTmux
    attr_reader :mutations, :new_session_args, :name_lookups

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @created = false
      @mutations = []
      @name_lookups = 0
    end

    def session(slug)
      @name_lookups += 1
      return unless @created && slug == @slug

      VpsfreeDevSession::Tmux::Session.new(
        id: '$replacement',
        name: @slug,
        mark: '',
        slug: '',
        workspace: @workspace
      )
    end

    def session_by_id(_id)
      nil
    end

    def capture(*args, allow_failure: false)
      raise "unexpected allow_failure: #{args.inspect}" if allow_failure
      raise "unexpected tmux capture: #{args.inspect}" unless args.first == 'new-session'

      @created = true
      @new_session_args = args
      ["$original\n", '', nil]
    end

    def run(*args)
      @mutations << args
    end
  end

  class ReplacedBeforeSyncTmux < NullTmux
    attr_reader :mutations, :name_lookups

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @created = false
      @id_lookups = 0
      @mutations = []
      @name_lookups = 0
      @pane = 0
    end

    def session(slug)
      @name_lookups += 1
      return unless @created && slug == @slug

      identity('$replacement', managed: true)
    end

    def session_by_id(id)
      return identity(id, managed: true) if id == '$replacement'
      return unless id == '$original'

      @id_lookups += 1
      case @id_lookups
      when 1 then identity(id, managed: false)
      when 2 then identity(id, managed: true)
      end
    end

    def capture(*args, allow_failure: false)
      raise "unexpected allow_failure: #{args.inspect}" if allow_failure

      output = case args.first
               when 'new-session'
                 @created = true
                 '$original'
               when 'display-message'
                 '%left'
               when 'split-window'
                 @pane += 1
                 "%pane#{@pane}"
               else
                 raise "unexpected tmux capture: #{args.inspect}"
               end
      ["#{output}\n", '', nil]
    end

    def run(*args)
      @mutations << args
    end

    def windows(_session)
      []
    end

    private

    def identity(id, managed:)
      VpsfreeDevSession::Tmux::Session.new(
        id:,
        name: @slug,
        mark: managed ? '1' : '',
        slug: managed ? @slug : '',
        workspace: @workspace
      )
    end
  end

  class PartialCreateTmux < NullTmux
    attr_reader :kill_count, :split_attempts

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @created = false
      @kill_count = 0
      @split_attempts = 0
      @mark = ''
      @session_slug = ''
    end

    def session(slug)
      return unless @created && slug == @slug

      identity
    end

    def session_by_id(id)
      return unless @created && id == '$partial'

      identity
    end

    def capture(*args, allow_failure: false)
      raise "unexpected allow_failure: #{args.inspect}" if allow_failure

      case args.first
      when 'new-session'
        @created = true
        @mark = ''
        @session_slug = ''
        ["$partial\n", '', nil]
      when 'display-message'
        ["%left\n", '', nil]
      when 'split-window'
        @split_attempts += 1
        raise VpsfreeDevSession::Error, 'split failed'
      else
        raise "unexpected tmux capture: #{args.inspect}"
      end
    end

    def run(*args)
      if args.first == 'set-option' && args[-2] == VpsfreeDevSession::SESSION_MARK
        @mark = args.last
      elsif args.first == 'set-option' && args[-2] == VpsfreeDevSession::SESSION_SLUG
        @session_slug = args.last
      elsif args.first == 'kill-session'
        @created = false
        @kill_count += 1
      end
    end

    private

    def identity
      VpsfreeDevSession::Tmux::Session.new(
        id: '$partial',
        name: @slug,
        mark: @mark,
        slug: @session_slug,
        workspace: @workspace,
        environment_slug: @slug
      )
    end
  end

  class RecordingTmux < NullTmux
    attr_reader :mutations

    def initialize
      @mutations = []
    end

    def run(*arguments)
      @mutations << arguments
    end
  end

  class CallbackCommandRunner
    def initialize(out:, err:, &callback)
      @delegate = VpsfreeDevSession::CommandRunner.new(out:, err:)
      @callback = callback
    end

    def capture(argv, allow_failure: false)
      @callback.call(argv)
      @delegate.capture(argv, allow_failure:)
    end

    def run(argv)
      @callback.call(argv)
      @delegate.run(argv)
    end
  end

  TODAY = Date.new(2026, 6, 6)

  def test_build_slug_prefixes_current_date
    with_workspace do |workspace|
      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-api-token-rotation',
        runner.build_slug('api-token-rotation', as_is: false)
      )
      assert_equal(
        '2026-05-31-api-token-rotation',
        runner.build_slug('2026-05-31-api-token-rotation', as_is: true)
      )
    end
  end

  def test_fork_creates_a_conversation_only_session
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup_runner = runner_for(workspace)
      setup_runner.ensure_tracking_files(source_slug)
      source_manifest = setup_runner.send(:ensure_portal_manifest, source_slug)
      source_manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup_runner.send(:write_portal_manifest, source_slug, source_manifest)

      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{log.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        puts JSON.generate(threadId: 'thread-fork') if ARGV[0, 2] == ['thread', 'fork']
      RUBY
      session = VpsfreeDevSession::Tmux::Session.new(
        id: '$fork', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_kwargs| session }
        define_method(:sync_slug) { |*_args, **_kwargs| session }
      end
      out = StringIO.new
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, portal_command: [RbConfig.ruby, portal],
        out:, err: StringIO.new, today: TODAY, env: {}
      )

      runner.fork(
        source_slug, 'alternative', as_is: false, json: true,
        model: 'gpt-test', effort: 'xhigh'
      )

      result = JSON.parse(out.string)
      assert_equal(destination_slug, result.fetch('slug'))
      assert_equal(source_slug, result.fetch('forkedFrom'))
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, manifest.fetch('forked_from'))
      assert_equal('thread-fork', manifest.dig('codex', 'thread_id'))
      assert_empty(manifest.fetch('repositories'))
      assert_empty(manifest.fetch('artifacts'))
      assert_empty(Dir.children(File.join(workspace, 'worktrees', destination_slug)))
      command = File.readlines(log, chomp: true).find { |line| line.start_with?('thread fork ') }
      assert_includes(command, '--thread-id thread-source')
      assert_includes(command, '--model gpt-test')
      assert_includes(command, '--effort xhigh')
    end
  end

  def test_fork_refuses_an_existing_destination
    with_workspace do |workspace|
      runner = runner_for(workspace)
      runner.ensure_tracking_files('2026-06-05-source')
      manifest = runner.send(:ensure_portal_manifest, '2026-06-05-source')
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      runner.send(:write_portal_manifest, '2026-06-05-source', manifest)
      runner.ensure_tracking_files('2026-06-06-taken')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.fork('2026-06-05-source', 'taken', as_is: false, json: true)
      end
      assert_includes(error.message, 'already exists')
    end
  end

  def test_fork_resumes_after_thread_creation_and_tmux_failure
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup_runner = runner_for(workspace)
      setup_runner.ensure_tracking_files(source_slug)
      manifest = setup_runner.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup_runner.send(:write_portal_manifest, source_slug, manifest)
      calls = File.join(workspace, 'fork-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        if ARGV[0, 2] == ['thread', 'fork']
          File.open(#{calls.dump}, 'a') { |file| file.puts 'fork' }
          puts JSON.generate(threadId: 'thread-fork')
        end
      RUBY
      failing_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) do |*_args, **_kwargs|
          raise VpsfreeDevSession::Error, 'tmux failed'
        end
      end
      failing = failing_class.new(
        workspace:, tmux: NullTmux.new, portal_command: [RbConfig.ruby, portal],
        out: StringIO.new, err: StringIO.new, today: TODAY, env: {}
      )
      assert_raises(VpsfreeDevSession::Error) do
        failing.fork(source_slug, 'retry', as_is: false, json: true)
      end

      session = VpsfreeDevSession::Tmux::Session.new(
        id: '$fork', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      retry_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_kwargs| session }
        define_method(:sync_slug) { |*_args, **_kwargs| session }
      end
      retry_runner = retry_class.new(
        workspace:, tmux: NullTmux.new, portal_command: [RbConfig.ruby, portal],
        out: StringIO.new, err: StringIO.new, today: TODAY, env: {}
      )
      retry_runner.fork(source_slug, 'retry', as_is: false, json: true)
      assert_equal(["fork\n"], File.readlines(calls))
    end
  end

  def test_slug_validation_rejects_paths_and_tmux_targets
    with_workspace do |workspace|
      runner = runner_for(workspace)

      assert_raises(VpsfreeDevSession::Error) do
        runner.build_slug('../escape', as_is: false)
      end

      assert_raises(VpsfreeDevSession::Error) do
        runner.build_slug('demo:1', as_is: false)
      end
    end
  end

  def test_lookup_slug_reports_ambiguity
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-demo'))
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', '2026-06-06-demo'))

      runner = runner_for(workspace)
      error = assert_raises(VpsfreeDevSession::Error) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
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
      error = assert_raises(VpsfreeDevSession::Error) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.resolve_start_slug('demo', as_is: true, new: true)
      end

      assert_match(/cannot be used together/, error.message)
    end
  end

  def test_start_new_rejects_dated_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.resolve_start_slug('2026-06-05-demo', as_is: false, new: true)
      end

      assert_match(/requires a short name/, error.message)
    end
  end

  def test_start_as_is_rejects_an_unsafe_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.resolve_start_slug('../escape', as_is: true, new: false)
      end

      assert_match(/invalid slug/, error.message)
    end
  end

  def test_start_rejects_json_attach_before_creating_session_state
    with_workspace do |workspace|
      slug = '2026-06-06-demo'

      error = assert_raises(VpsfreeDevSession::Error) do
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
      runner = VpsfreeDevSession::Runner.new(
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
      socket = '/run/vpsfree-workspace-tmux/tmux.sock'
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'VPSFREE_DEV_SESSION_TMUX_SOCKET' => socket }
      )

      assert_equal(socket, runner.instance_variable_get(:@tmux).socket)
    end
  end

  def test_portal_runtime_mode_fails_closed_and_propagates_to_sessions
    with_workspace do |workspace|
      error = assert_raises(VpsfreeDevSession::Error) do
        VpsfreeDevSession::Runner.new(
          workspace:,
          tmux: NullTmux.new,
          require_runtime: true,
          out: StringIO.new,
          err: StringIO.new,
          env: {}
        )
      end
      assert_includes(error.message, 'portal runtime configuration is incomplete')

      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        authority_dir: '/run/workspace-authority',
        tmux_socket: '/run/workspace-tmux/tmux.sock',
        codex_socket: '/run/workspace-codex/app-server.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        portal_command: ['/run/current-system/sw/bin/workspace-portal'],
        require_runtime: true,
        out: StringIO.new,
        err: StringIO.new,
        env: {}
      )
      environment = runner.send(:session_environment, '2026-06-06-demo')
      contract = JSON.parse(
        File.read(File.expand_path('../portal/runtime-contract.json', __dir__))
      )
      assert_equal(
        VpsfreeDevSession::MAX_MESSAGE_BYTES,
        contract.fetch('maxMessageBytes')
      )
      environment_keys = contract.fetch('threadEnvironmentKeys')
      assert_equal(environment_keys.sort, environment.keys.sort)
      assert_equal(
        (environment_keys - [VpsfreeDevSession::ENV_REQUIRE_RUNTIME]).sort,
        VpsfreeDevSession::THREAD_ENV_ARGUMENTS.keys.sort
      )
      assert_equal('1', environment.fetch(VpsfreeDevSession::ENV_REQUIRE_RUNTIME))
      assert_equal(
        VpsfreeDevSession::DEFAULT_PORTAL_BASE_URL,
        environment.fetch(VpsfreeDevSession::ENV_PORTAL_BASE_URL)
      )
      assert_equal(
        "#{VpsfreeDevSession::DEFAULT_PORTAL_BASE_URL}/2026-06-06-demo/",
        environment.fetch(VpsfreeDevSession::ENV_PORTAL_URL)
      )
      assert_equal(
        '/run/current-system/sw/bin/workspace-portal',
        environment.fetch(VpsfreeDevSession::ENV_PORTAL_COMMAND)
      )
    end
  end

  def test_deployment_runtime_flags_are_accepted
    with_workspace do |workspace|
      contract = JSON.parse(
        File.read(File.expand_path('../portal/runtime-contract.json', __dir__))
      )
      values = {
        '--workspace' => workspace,
        '--authority-dir' => File.join(workspace, 'authority'),
        '--tmux-socket' => File.join(workspace, 'tmux.sock'),
        '--codex-command' => '/bin/true',
        '--codex-socket' => File.join(workspace, 'codex.sock'),
        '--codex-version' => 'test-version',
        '--portal-command' => '/bin/true',
        '--portal-base-url' => 'https://workspace.example.test'
      }
      arguments = contract.fetch('devSessionFlags').flat_map do |option|
        option == '--require-runtime' ? [option] : [option, values.fetch(option)]
      end
      out = StringIO.new
      err = StringIO.new
      status = VpsfreeDevSession::CLI.new(arguments + ['validate'], out:, err:).run
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
      status = VpsfreeDevSession::CLI.new(arguments + ['--help'], out:, err:).run
      assert_equal(0, status, err.string)
      assert_includes(out.string, 'Usage:')

      %w[--workspace --workspace=/tmp/caller].each do |override|
        caller_arguments = override == '--workspace' ? [override, '/tmp/caller'] : [override]
        out = StringIO.new
        err = StringIO.new
        status = VpsfreeDevSession::CLI.new(
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
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        env: {
          VpsfreeDevSession::ENV_PORTAL_BASE_URL => 'https://workspace.example.test',
          VpsfreeDevSession::ENV_PORTAL_URL => "https://workspace.example.test/#{slug}/"
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
          runner = VpsfreeDevSession::Runner.new(
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
          assert(File.file?(authority_lock))
          assert_equal(0o600, File.stat(authority_lock).mode & 0o777)
          _stdout, _stderr, status = Open3.capture3(
            { 'TMUX_TMPDIR' => File.join(socket_directory, 'elsewhere') },
            'tmux', '-S', socket, 'has-session', '-t', "=#{slug}:"
          )
          assert(status.success?, 'explicit tmux socket did not survive TMUX_TMPDIR drift')

          out = StringIO.new
          ordinary_runner = VpsfreeDevSession::Runner.new(
            workspace:,
            authority_dir:,
            out:,
            err: StringIO.new,
            today: TODAY,
            env: {}
          )
          ordinary_runner.list(slug, as_is: true)
          assert_includes(out.string, 'managed')

          mismatch = VpsfreeDevSession::Runner.new(
            workspace:,
            tmux_socket: File.join(socket_directory, 'other.sock'),
            authority_dir:,
            out: StringIO.new,
            err: StringIO.new,
            today: TODAY,
            env: {}
          )
          error = assert_raises(VpsfreeDevSession::Error) do
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
        socket_path: '/run/vpsfree-workspace-tmux/tmux.sock'
      )
      calls = []
      runner = VpsfreeDevSession::Runner.new(
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
        socket_path: '/run/vpsfree-workspace-tmux/tmux.sock'
      )
      calls = []
      runner = VpsfreeDevSession::Runner.new(
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

  def test_start_seeds_the_goal_and_returns_json_with_a_shared_thread
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Implement a useful feature.\n")
      tmux = ManagedTmux.new(slug, workspace:)
      portal_command = [
        RbConfig.ruby,
        '-e',
        "require 'json'; puts JSON.generate(threadId: 'thread-123')"
      ]
      session = VpsfreeDevSession::Tmux::Session.new(
        id: '$created',
        name: slug,
        mark: '1',
        slug:,
        workspace:
      )
      runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) do |_slug, run_codex:, thread_id:|
          raise 'missing shared thread' unless run_codex && thread_id == 'thread-123'

          session
        end

        define_method(:sync_slug) do |_slug, require_session:, session:|
          raise 'missing created session' unless require_session && session

          session
        end

        define_method(:revalidate_session!) do |expected|
          expected
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command:,
        portal_url: 'https://workspace.example.test'
      )

      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true
      )

      result = JSON.parse(out.string)
      assert_equal(slug, result['slug'])
      assert_equal('thread-123', result['threadId'])
      assert_equal("https://workspace.example.test/#{slug}/", result['url'])
      assert_includes(File.read(File.join(workspace, 'work', slug, 'plan.md')), 'Implement a useful feature.')
      assert_includes(File.read(File.join(workspace, 'work', slug, 'state.md')), 'initial browser request')
      assert_equal(
        {
          'state' => 'ready',
          'initial_goal_sent' => true,
          'goal_sha256' => Digest::SHA256.hexdigest('Implement a useful feature.')
        },
        YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
            .fetch('creation')
      )
    end
  end

  def test_stopped_ready_session_opens_only_its_recorded_thread
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
        warn 'recorded thread is unavailable'
        exit 1
      RUBY
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = { 'thread_id' => 'thread-ready' }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(VpsfreeDevSession::CommandError) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          json: true
        )
      end
      assert_includes(error.message, 'recorded thread is unavailable')
      command = File.read(log)
      assert_includes(command, 'thread create')
      assert_includes(command, '--thread-id thread-ready')
      refute_includes(command, '--recover-creating')
      unchanged = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-ready', unchanged.dig('codex', 'thread_id'))
      assert_equal('ready', unchanged.dig('creation', 'state'))
    end
  end

  def test_start_refuses_to_retrofit_an_unshared_running_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ManagedTmux.new(slug, workspace:)
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: nil
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      shared_runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [
          RbConfig.ruby,
          '-e',
          "require 'json'; puts JSON.generate(threadId: 'unexpected')"
        ]
      )
      error = assert_raises(VpsfreeDevSession::Error) do
        shared_runner.start(slug, as_is: true, new: false, attach: false, run_codex: true)
      end
      assert_match(/no shared Codex thread/, error.message)
    end
  end

  def test_goal_seeding_is_stable_when_goal_contains_template_headings
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Keep this literal text:\n\n## Goal\n\n## Affected repositories\n")
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)

      runner.send(:seed_goal, slug, goal)
      first_plan = File.binread(File.join(workspace, 'work', slug, 'plan.md'))
      first_state = File.binread(File.join(workspace, 'work', slug, 'state.md'))
      runner.send(:seed_goal, slug, goal)

      assert_equal(first_plan, File.binread(File.join(workspace, 'work', slug, 'plan.md')))
      assert_equal(first_state, File.binread(File.join(workspace, 'work', slug, 'state.md')))
      assert_equal(1, first_plan.scan('Keep this literal text:').length)
    end
  end

  def test_exclusive_browser_start_retries_partial_creation_without_duplicate_thread
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      failure_marker = File.join(workspace, 'send-failed')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "Implement a retry-safe feature.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        case ARGV[1]
        when 'create'
          puts JSON.generate(threadId: 'thread-retry')
        when 'ensure-initial'
          unless File.exist?(#{failure_marker.dump})
            File.write(#{failure_marker.dump}, "failed\n")
            warn 'simulated send failure'
            exit 1
          end
        end
      RUBY
      out = StringIO.new
      created_session = VpsfreeDevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock'
      )
      runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **_keywords|
          created_session
        end

        define_method(:sync_slug) do |*_arguments, **_keywords|
          created_session
        end

        define_method(:revalidate_session!) do |_expected|
          created_session
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )

      assert_raises(VpsfreeDevSession::CommandError) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end
      partial = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-retry', partial.dig('codex', 'thread_id'))
      assert_equal(2, partial.fetch('schema'))
      assert_equal('creating', partial.dig('creation', 'state'))
      refute(partial.dig('creation', 'initial_goal_sent'))
      assert(partial.dig('creation', 'initial_goal_attempted'))

      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )
      commands = File.readlines(log, chomp: true)
      creation_commands = commands.select { |line| line.start_with?('thread create ') }
      assert_equal(2, creation_commands.length)
      creation_commands.each { |line| assert_includes(line, '--recover-creating') }
      refute_includes(creation_commands.fetch(0), '--thread-id')
      assert_includes(creation_commands.fetch(1), '--thread-id thread-retry')
      assert_equal(1, commands.count { |line| line.start_with?('thread set-name ') })
      initial_commands = commands.select { |line| line.start_with?('thread ensure-initial ') }
      assert_equal(2, initial_commands.length)
      assert_includes(initial_commands.fetch(0), '--start-unmaterialized')
      refute_includes(initial_commands.fetch(1), '--start-unmaterialized')
      complete = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', complete.dig('creation', 'state'))
      assert(complete.dig('creation', 'initial_goal_sent'))
      assert_equal(1, complete.fetch('schema'))
      refute(complete.fetch('creation').key?('initial_goal_attempted'))

      out.truncate(0)
      out.rewind
      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )
      assert_equal('thread-retry', JSON.parse(out.string).fetch('threadId'))
      replayed_commands = File.readlines(log, chomp: true)
      assert_equal(2, replayed_commands.count { |line| line.start_with?('thread create ') })
      assert_equal(1, replayed_commands.count { |line| line.start_with?('thread set-name ') })
      assert_equal(2, replayed_commands.count { |line| line.start_with?('thread ensure-initial ') })
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal('ready', journal.fetch('state'))

      replay_out = StringIO.new
      replay_runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out: replay_out,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      replay_runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )
      replay = JSON.parse(replay_out.string)
      assert_equal('thread-retry', replay.fetch('threadId'))
      assert_nil(replay.fetch('attach'))
      assert_equal(replayed_commands, File.readlines(log, chomp: true))
    end
  end

  def test_initial_turn_starts_after_private_local_creation_and_without_the_slug_lock
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      authority_dir = File.join(workspace, 'runtime-authority')
      observation = File.join(workspace, 'initial-turn-observation')
      portal = File.join(workspace, 'fake-portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(goal, "Implement after local setup.\n")
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        require 'yaml'
        case ARGV[1]
        when 'create'
          puts JSON.generate(threadId: 'thread-123')
        when 'ensure-initial'
          authority = JSON.parse(File.read(#{File.join(authority_dir, "#{slug}.json").dump}))
          manifest = YAML.safe_load(File.read(#{File.join(workspace, 'work', slug, 'portal.yml').dump}))
          exit 2 unless authority['state'] == 'creating'
          exit 3 unless manifest.dig('creation', 'state') == 'creating'
          File.open(#{File.join(authority_dir, "#{slug}.lock").dump}, File::RDWR) do |lock|
            exit 4 unless lock.flock(File::LOCK_EX | File::LOCK_NB)
          end
          File.write(#{observation.dump}, "ready for initial turn\n")
        end
      RUBY
      session = VpsfreeDevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-123',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }
      end
      runner = runner_class.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      runner.start(
        slug, as_is: true, new: false, attach: false, run_codex: true,
        goal_file: goal, json: true, exclusive: true
      )

      assert_equal("ready for initial turn\n", File.read(observation))
      authority = JSON.parse(File.read(File.join(authority_dir, "#{slug}.json")))
      assert_equal('ready', authority.fetch('state'))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert(manifest.dig('creation', 'initial_goal_sent'))
    end
  end

  def test_exclusive_browser_retry_replaces_unmaterialized_thread_after_app_server_restart
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      codex = File.join(workspace, 'codex')
      authority_dir = File.join(workspace, 'runtime-authority')
      File.write(goal, "Recover after an App Server restart.\n")
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.0'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        case ARGV[1]
        when 'create'
          id = ARGV.include?('--thread-id') ? 'thread-replacement' : 'thread-vanished'
          puts JSON.generate(threadId: id)
        when 'ensure-initial'
          id = ARGV.fetch(ARGV.index('--thread-id') + 1)
          exit 1 if id == 'thread-vanished'
        end
      RUBY
      original = VpsfreeDevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-vanished', codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.153.0', codex_pane_id: '%1'
      )
      first_runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| original }
        define_method(:sync_slug) { |*_arguments, **_keywords| original }
        define_method(:revalidate_session!) { |_expected| original }
      end
      first = first_runner_class.new(
        workspace:, authority_dir:, tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock', codex_version: '0.153.0',
        codex_command: codex, tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      assert_raises(VpsfreeDevSession::CommandError) do
        first.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end

      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-vanished', codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.153.0', codex_pane_id: '%1', id: '$7'
      )
      replacement = original.dup
      replacement.id = '$8'
      replacement.codex_thread_id = 'thread-replacement'
      second_runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| replacement }
        define_method(:sync_slug) { |*_arguments, **_keywords| replacement }
        define_method(:revalidate_session!) { |expected| expected }
      end
      second = second_runner_class.new(
        workspace:, authority_dir:, tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock', codex_version: '0.153.0',
        codex_command: codex, tmux:,
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      second.start(
        slug, as_is: true, new: false, attach: false, run_codex: true,
        goal_file: goal, json: true, exclusive: true
      )

      assert(tmux.killed)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-replacement', manifest.dig('codex', 'thread_id'))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert(manifest.dig('creation', 'initial_goal_sent'))
      authority = JSON.parse(File.read(File.join(authority_dir, "#{slug}.json")))
      assert_equal('$8', authority.fetch('tmux_session_id'))
      assert_equal('thread-replacement', authority.fetch('codex_thread_id'))
      assert_equal('ready', authority.fetch('state'))
      commands = File.readlines(log, chomp: true)
      assert_equal(2, commands.count { |line| line.start_with?('thread create ') })
      commands.select { |line| line.start_with?('thread create ') }.each do |line|
        assert_includes(line, '--recover-creating')
      end
      assert(commands.any? do |line|
        line.start_with?('thread ensure-initial ') &&
          line.include?('--thread-id thread-replacement') &&
          line.include?("--cwd #{File.join(workspace, 'work', slug)}")
      end)
    end
  end

  def test_exclusive_browser_replay_repairs_authority_after_ready_journal_crash
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      authority_dir = File.join(workspace, 'runtime-authority')
      portal = File.join(workspace, 'fake-portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(goal, "Complete the recoverable initial turn.\n")
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        puts JSON.generate(threadId: 'thread-crash') if ARGV[1] == 'create'
      RUBY
      session = VpsfreeDevSession::Tmux::Session.new(
        id: '$8', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-crash',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      crashing_runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }

        def mark_creation_journal_ready(slug, journal)
          super
          raise VpsfreeDevSession::Error, 'simulated crash before authority publication'
        end
      end
      runner = crashing_runner_class.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      assert_raises(VpsfreeDevSession::Error) do
        runner.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      authority_path = File.join(authority_dir, "#{slug}.json")
      authority = JSON.parse(File.read(authority_path))
      assert_equal('ready', journal.fetch('state'))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert_equal('creating', authority.fetch('state'))

      out = StringIO.new
      replay = VpsfreeDevSession::Runner.new(
        workspace:,
        authority_dir:,
        tmux: ManagedTmux.new(
          slug,
          workspace:,
          socket_path: '/run/test/tmux.sock',
          codex_thread_id: 'thread-crash',
          codex_socket_path: '/run/test/codex.sock',
          codex_client_version: '0.152.1',
          id: '$8'
        ),
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      replay.start(
        slug, as_is: true, new: false, attach: false, run_codex: true,
        goal_file: goal, json: true, exclusive: true
      )

      assert_equal('thread-crash', JSON.parse(out.string).fetch('threadId'))
      repaired = JSON.parse(File.read(authority_path))
      assert_equal('ready', repaired.fetch('state'))
    end
  end

  def test_exclusive_browser_start_recovers_a_lost_thread_result_and_rejects_goal_changes
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      changed_goal = File.join(workspace, 'changed-goal.txt')
      thread_marker = File.join(workspace, 'thread-created')
      invocation_log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "Implement the original request.\n")
      File.write(changed_goal, "Implement a different request.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{invocation_log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        case ARGV[1]
        when 'create'
          unless File.exist?(#{thread_marker.dump})
            File.write(#{thread_marker.dump}, "thread-recovered\n")
            warn 'simulated loss after App Server committed thread/start'
            exit 1
          end
          puts JSON.generate(threadId: File.read(#{thread_marker.dump}).strip)
        end
      RUBY
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )

      assert_raises(VpsfreeDevSession::CommandError) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end
      journal = runner.send(:creation_journal_file, slug)
      assert(File.file?(journal))
      partial = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('creating', partial.dig('creation', 'state'))
      assert_nil(partial.dig('codex', 'thread_id'))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: changed_goal,
          json: true,
          exclusive: true
        )
      end
      assert_includes(error.message, 'does not match the recorded goal')

      retry_runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      retry_runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )

      complete = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-recovered', complete.dig('codex', 'thread_id'))
      assert_equal('ready', complete.dig('creation', 'state'))
      assert_equal('ready', JSON.parse(File.read(journal)).fetch('state'))
      creation_commands = File.readlines(invocation_log, chomp: true)
                              .select { |line| line.include?('thread create') }
      assert_equal(2, creation_commands.length)
      creation_commands.each do |command|
        assert_includes(command, '--recover-creating')
        assert_includes(command, "--cwd #{File.join(workspace, 'work', slug)}")
        assert_includes(command, "--workspace #{workspace}")
        assert_includes(command, "--session-slug #{slug}")
        assert_includes(command, "--worktrees-dir #{File.join(workspace, 'worktrees', slug)}")
        assert_includes(command, '--portal-base-url https://vpsfree-cz-workspace.aitherdev.int.vpsfree.cz')
        assert_includes(command, "--portal-url https://vpsfree-cz-workspace.aitherdev.int.vpsfree.cz/#{slug}/")
      end
      assert_equal('thread-recovered', File.read(thread_marker).strip)
    end
  end

  def test_exclusive_browser_start_journals_before_tracking_files
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume after an early crash.\n")
      crashing_runner_class = Class.new(VpsfreeDevSession::Runner) do
        def ensure_tracking_files(slug)
          super
          raise VpsfreeDevSession::Error, 'simulated crash after tracking creation'
        end
      end
      crashing_runner = crashing_runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, '-e', "require 'json'; puts JSON.generate(threadId: 'thread-early')"]
      )

      assert_raises(VpsfreeDevSession::Error) do
        crashing_runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end
      journal = crashing_runner.send(:creation_journal_file, slug)
      assert(File.file?(journal))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))

      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, '-e', "require 'json'; puts JSON.generate(threadId: 'thread-early')"]
      )
      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert_equal('thread-early', manifest.dig('codex', 'thread_id'))
      assert_equal('ready', JSON.parse(File.read(journal)).fetch('state'))
    end
  end

  def test_exclusive_browser_retry_refuses_partial_tracking_content
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume after an early crash.\n")
      crashing_runner_class = Class.new(VpsfreeDevSession::Runner) do
        def ensure_tracking_files(_slug)
          raise VpsfreeDevSession::Error, 'simulated crash before tracking creation'
        end
      end
      crashing_runner = crashing_runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      assert_raises(VpsfreeDevSession::Error) do
        crashing_runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end

      runner = runner_for(workspace)
      tracking = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(tracking)
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug))
      File.write(File.join(tracking, 'plan.md'), "# #{slug}\n\n## Goal\n")
      File.write(File.join(tracking, 'state.md'), runner.send(:state_skeleton, slug))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end
      assert_match(/incomplete and cannot be reconciled/, error.message)
    end
  end

  def test_ruby_manifest_validator_accepts_shared_fixture
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      directory = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(directory)
      FileUtils.cp(
        File.expand_path('fixtures/portal-manifest-valid.yml', __dir__),
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
    fixtures = Dir[File.expand_path('fixtures/portal-manifest-valid*.yml', __dir__)]
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

  def test_validate_checks_all_persisted_portal_manifests
    with_workspace do |workspace|
      valid_directory = File.join(workspace, 'work', '2026-09-03-example')
      invalid_directory = File.join(workspace, 'archive', '2026-09-03-invalid')
      FileUtils.mkdir_p(valid_directory)
      FileUtils.mkdir_p(invalid_directory)
      FileUtils.cp(
        File.expand_path('fixtures/portal-manifest-valid.yml', __dir__),
        File.join(valid_directory, 'portal.yml')
      )
      File.write(
        File.join(invalid_directory, 'portal.yml'),
        "schema: 1\nslug: 2026-09-03-other\n"
      )

      assert_raises(VpsfreeDevSession::Error) do
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
      error = assert_raises(VpsfreeDevSession::Error) do
        validating_runner.validate
      end
      assert_includes(error.message, 'duplicate session')

      FileUtils.rm_rf(File.join(workspace, 'work', slug))
      File.write(File.join(archive, 'state.md'), "---\nlifecycle: active\n---\n")
      error = assert_raises(VpsfreeDevSession::Error) do
        validating_runner.validate
      end
      assert_includes(error.message, 'terminal lifecycle')
    end
  end

  def test_ruby_manifest_validator_rejects_shared_invalid_fixtures
    fixtures = Dir[File.expand_path('fixtures/portal-manifest-invalid-*.yml', __dir__)]
    refute_empty(fixtures)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        slug = '2026-09-03-example'
        directory = File.join(workspace, 'work', slug)
        FileUtils.mkdir_p(directory)
        FileUtils.cp(fixture, File.join(directory, 'portal.yml'))

        assert_raises(VpsfreeDevSession::Error, File.basename(fixture)) do
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
    Dir[File.expand_path('fixtures/runtime-authority-valid-*.json', __dir__)].each do |fixture|
      record = JSON.parse(File.read(fixture))
      record['workspace'] = runner.workspace
      runner.send(:validate_session_authority!, record, 'example', fixture)
    end
    Dir[File.expand_path('fixtures/runtime-authority-invalid-*.json', __dir__)].each do |fixture|
      record = JSON.parse(File.read(fixture))
      record['workspace'] = runner.workspace
      assert_raises(VpsfreeDevSession::Error, File.basename(fixture)) do
        runner.send(:validate_session_authority!, record, 'example', fixture)
      end
    end
  end

  def test_exclusive_start_refuses_to_reuse_an_existing_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'
      runner.ensure_tracking_files(slug)

      error = assert_raises(VpsfreeDevSession::Error) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
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

      assert_raises(VpsfreeDevSession::CommandError) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
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

      worktree = File.join(workspace, 'worktrees', long_slug, 'vpsadmin')
      FileUtils.mkdir_p(worktree)
      File.write(File.join(worktree, '.git'), "gitdir: /tmp/nonexistent\n")

      out = StringIO.new
      runner = VpsfreeDevSession::Runner.new(
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
          VpsfreeDevSession::ENV_SLUG => slug,
          VpsfreeDevSession::ENV_WORKSPACE => workspace
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
            VpsfreeDevSession::ENV_SLUG => '2026-06-06-demo',
            VpsfreeDevSession::ENV_WORKSPACE => foreign_workspace
          }
        )

        error = assert_raises(VpsfreeDevSession::Error) { runner.current }

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

        error = assert_raises(VpsfreeDevSession::Error) { runner.current }

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
      cwd = File.join(workspace, 'worktrees', slug, 'vpsadmin', 'app')
      FileUtils.mkdir_p(cwd)

      runner = runner_for(workspace, cwd:)

      assert_equal(slug, runner.current_slug)
    end
  end

  def test_current_reports_missing_active_session
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(VpsfreeDevSession::Error) do
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
      runner = VpsfreeDevSession::Runner.new(
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

      error = assert_raises(VpsfreeDevSession::Error) { runner.current }

      assert_match(/no current dev session/, error.message)
      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_devcluster_shorthands_use_the_workspace_session_resolver
    scripts = %w[
      dev-clusters/vpsadmin/bin/devcluster
      dev-clusters/vpsadminos/bin/devcluster
    ]

    Dir.mktmpdir('dev-session-resolver') do |directory|
      resolver = File.join(directory, 'dev-session')
      File.write(
        resolver,
        "#!/bin/sh\n" \
        "test \"$1\" = current\n" \
        "printf 'managed-session\\n'\n"
      )
      FileUtils.chmod(0o755, resolver)

      scripts.each do |relative_path|
        source = File.read(File.expand_path("../#{relative_path}", __dir__))
        function = source[/^current_slug\(\) \{\n.*?^\}\n/m]
        refute_nil(function, relative_path)

        stdout, stderr, status = Open3.capture3(
          { 'VPSFREE_DEV_SESSION_SLUG' => 'foreign-session' },
          'bash',
          '-c',
          "#{function}\nDEV_SESSION_BIN=\"$1\"\ncurrent_slug\n",
          'devcluster-current-slug-test',
          resolver
        )

        assert(status.success?, "#{relative_path}: #{stderr}")
        assert_equal("managed-session\n", stdout, relative_path)
      end
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
          VpsfreeDevSession::ENV_SLUG => slug,
          VpsfreeDevSession::ENV_WORKSPACE => workspace
        },
        cwd:
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.current
      end

      assert_match(/sources disagree/, error.message)
      assert_match(/#{VpsfreeDevSession::ENV_SLUG}=#{slug}/, error.message)
      assert_match(/cwd=#{other_slug}/, error.message)
    end
  end

  def test_worktree_add_and_remove_keep_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      path = File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
      assert(File.exist?(File.join(path, '.git')))

      runner.worktree_remove('demo', 'sample', as_is: false, force: false)

      refute(File.exist?(path))
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )
    end
  end

  def test_worktree_add_records_github_comparison_metadata
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git',
        "--git-dir=#{repository}",
        'remote',
        'set-url',
        'origin',
        'git@github.com:vpsfreecz/sample.git'
      )

      runner_for(workspace).worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', '2026-06-06-demo', 'portal.yml'))
      )
      metadata = manifest.fetch('repositories').fetch(0)
      assert_equal('sample', metadata['name'])
      assert_equal('sample', metadata['project'])
      assert_equal('vpsfreecz/sample', metadata['github'])
      assert_equal('2026-06-06-demo', metadata['branch'])
      assert_equal('master', metadata['default_branch'])
      assert_match(/\A[0-9a-f]{40}\z/, metadata['initial_base_sha'])
    end
  end

  def test_worktree_add_uses_one_immutable_base_and_recovers_registration
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      base_sha = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', '-b', slug, path, base_sha
      )

      runner.worktree_add(
        slug,
        'sample',
        as_is: true,
        name: nil,
        branch: slug,
        base: 'master',
        fetch: false
      )

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      metadata = manifest.fetch('repositories').fetch(0)
      assert_equal(base_sha, metadata['initial_base_sha'])
      assert_equal(slug, metadata['branch'])
    end
  end

  def test_explicit_base_does_not_change_recorded_default_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success('git', "--git-dir=#{repository}", 'branch', 'release', 'master')

      runner_for(workspace).worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'release',
        fetch: false
      )

      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', '2026-06-06-demo', 'portal.yml'))
      )
      assert_equal('master', manifest.dig('repositories', 0, 'default_branch'))
    end
  end

  def test_workspace_repository_worktree_can_be_finalized_with_stable_identity
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      File.write(File.join(workspace, 'README.md'), "# Workspace\n")
      assert_git_success('git', '-C', workspace, 'add', 'README.md')
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'initial workspace')
      assert_git_success(
        'git', '-C', workspace, 'remote', 'add', 'origin',
        'git@github.com:vpsfreecz/vpsfree-cz-workspace.git'
      )
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug,
        'workspace',
        as_is: true,
        name: nil,
        branch: slug,
        base: 'master',
        fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'workspace')
      assert(File.exist?(File.join(path, '.git')))

      commit_tracking(workspace, slug, lifecycle: 'complete')
      runner.finalize(slug, as_is: true)

      refute(File.exist?(path))
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'archive', slug, 'portal.yml'))
      )
      repository = manifest.fetch('repositories').fetch(0)
      assert_equal('workspace', repository['name'])
      assert_equal('vpsfreecz/vpsfree-cz-workspace', repository['github'])
      assert_match(/\A[0-9a-f]{40}\z/, repository['final_head_sha'])
      assert_git_success(
        'git', '-C', workspace, 'show-ref', '--verify', '--quiet',
        "refs/heads/#{slug}"
      )
    end
  end

  def test_worktree_add_accepts_an_in_root_repository_alias
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample-storage')
      FileUtils.ln_s(
        'sample-storage.git',
        File.join(workspace, 'repos', 'sample.git')
      )

      runner_for(workspace).worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      assert(
        File.directory?(
          File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
        )
      )
    end
  end

  def test_worktree_add_refuses_an_external_repository_alias
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      Dir.mktmpdir('external-dev-session-repository') do |external|
        FileUtils.mkdir_p(File.join(external, 'repos'))
        create_bare_repo(external, 'sample')
        FileUtils.ln_s(
          File.join(external, 'repos', 'sample.git'),
          File.join(workspace, 'repos', 'sample.git')
        )
        commands = []
        command_runner = CallbackCommandRunner.new(
          out: StringIO.new,
          err: StringIO.new
        ) { |argv| commands << argv }
        runner = VpsfreeDevSession::Runner.new(
          workspace:,
          command_runner:,
          tmux: NullTmux.new,
          out: StringIO.new,
          err: StringIO.new,
          today: TODAY
        )

        error = assert_raises(VpsfreeDevSession::Error) do
          runner.worktree_add(
            'demo',
            'sample',
            as_is: false,
            name: nil,
            branch: nil,
            base: 'master',
            fetch: true
          )
        end

        assert_match(/outside the canonical repository root/, error.message)
        refute(commands.any? { |argv| argv.include?('fetch') })
        refute(
          File.exist?(
            File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
          )
        )
        refute_git_success(
          'git',
          "--git-dir=#{File.join(external, 'repos', 'sample.git')}",
          'show-ref',
          '--verify',
          '--quiet',
          'refs/heads/2026-06-06-demo'
        )
      end
    end
  end

  def test_remove_cleans_worktrees_but_keeps_notes_and_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      runner.remove('demo', as_is: false, force: false)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      assert(File.exist?(File.join(workspace, 'work', slug, 'plan.md')))
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )
    end
  end

  def test_remove_records_heads_so_finalize_can_archive_later
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      expected_head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip

      runner.remove('demo', as_is: false, force: false)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal(expected_head, manifest.dig('repositories', 0, 'final_head_sha'))

      commit_tracking(workspace, slug, lifecycle: 'complete')
      runner.finalize('demo', as_is: false)
      archived = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(expected_head, archived.dig('repositories', 0, 'final_head_sha'))
      assert(archived['finalized_at'])
    end
  end

  def test_finalize_rejects_worktree_swapped_from_another_canonical_repository
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'worktree',
        'remove',
        path
      )
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'other.git')}",
        'worktree',
        'add',
        '-b',
        'replacement',
        path,
        'master'
      )
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end
      assert_match(/repository identity does not match/, error.message)
      assert(File.directory?(path))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_remove_kills_session_after_worktrees_are_removed
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      add_runner = runner_for(workspace)
      add_runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        on_kill: -> { refute(File.exist?(path)) }
      )
      remove_runner = runner_for(workspace, tmux:)

      remove_runner.remove('demo', as_is: false, force: false)

      assert(tmux.killed)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_all_is_rejected
    with_workspace do |workspace|
      err = StringIO.new
      status = VpsfreeDevSession::CLI.new(
        ['--workspace', workspace, 'remove', 'demo', '--all'],
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_match(/invalid option: --all/, err.string)
    end
  end

  def test_remove_refuses_dirty_worktrees_without_force
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      path = File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.remove('demo', as_is: false, force: false)
      end

      assert_match(/uncommitted changes/, error.message)
      assert(File.exist?(path))
    end
  end

  def test_remove_force_cleans_dirty_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")

      runner.remove('demo', as_is: false, force: true)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_kills_managed_tmux_session
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      assert(tmux_session_exists?(socket, slug))

      runner.remove('demo', as_is: false, force: false)

      refute(tmux_session_exists?(socket, slug))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_remove_refuses_unmanaged_tmux_session
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      tmux_run(socket, 'new-session', '-d', '-s', slug, '-c', workspace)

      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.remove('demo', as_is: false, force: false)
      end

      assert_match(/not managed/, error.message)
      assert(tmux_session_exists?(socket, slug))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_finalize_archives_tracking_after_removing_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      add_runner = runner_for(workspace)
      add_runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      commit_tracking(workspace, slug, lifecycle: 'active')
      state = File.join(workspace, 'work', slug, 'state.md')
      tracking_paths = [
        File.join('work', slug),
        File.join('archive', slug)
      ]
      set_lifecycle(workspace, slug, 'complete')
      File.write(state, "#{File.read(state)}\nFinal result: passed\n")
      assert_equal(
        '1',
        git_capture_success(
          'git', '-C', workspace, 'rev-list', '--count', 'HEAD', '--', *tracking_paths
        ).strip
      )
      out = StringIO.new
      tmux = ManagedTmux.new(slug, workspace:)

      runner = runner_for(workspace, tmux:, out:)
      runner.finalize('demo', as_is: false)

      refute(tmux.killed)
      assert_includes(out.string, File.join(workspace, 'archive', slug))
      assert_includes(
        out.string,
        "stop after committing: dev-session stop #{slug} --as-is"
      )
      assert_includes(
        File.read(File.join(workspace, 'archive', slug, 'state.md')),
        'lifecycle: complete'
      )
      assert_includes(
        File.read(File.join(workspace, 'archive', slug, 'state.md')),
        'Final result: passed'
      )
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.stop(slug, as_is: true)
      end
      assert_match(/must be committed before stopping/, error.message)
      refute(tmux.killed)

      remove_error = assert_raises(VpsfreeDevSession::Error) do
        runner.remove(slug, as_is: true, force: false)
      end
      assert_match(/must be committed before stopping/, remove_error.message)
      refute(tmux.killed)

      commit_archive_move(workspace, slug)
      assert_equal(
        '2',
        git_capture_success(
          'git', '-C', workspace, 'rev-list', '--count', 'HEAD', '--', *tracking_paths
        ).strip
      )
      runner.stop(slug, as_is: true)
      assert(tmux.killed)
    end
  end

  def test_session_closing_rejects_ambiguous_or_missing_tracking
    skip 'git is not available' unless command_available?('git')

    %i[stop remove].each do |operation|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        base_runner = runner_for(workspace)
        base_runner.ensure_tracking_files(slug)
        commit_tracking(workspace, slug, lifecycle: 'complete')
        tmux = ManagedTmux.new(slug, workspace:)
        runner = runner_for(workspace, tmux:)
        runner.finalize(slug, as_is: true)
        FileUtils.mkdir_p(File.join(workspace, 'work', slug))

        error = assert_raises(VpsfreeDevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.remove(slug, as_is: true, force: false)
          end
        end

        assert_match(/active and archived tracking both exist/, error.message)
        refute(tmux.killed)
      end

      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        tmux = ManagedTmux.new(slug, workspace:)
        runner = runner_for(workspace, tmux:)

        error = assert_raises(VpsfreeDevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.remove(slug, as_is: true, force: false)
          end
        end

        assert_match(/session tracking is missing/, error.message)
        refute(tmux.killed)
      end
    end
  end

  def test_session_closing_rejects_invalid_active_tracking
    %i[stop remove].product(%i[symlink file empty_directory]).each do |operation, kind|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        path = File.join(workspace, 'work', slug)
        case kind
        when :symlink
          FileUtils.ln_s(File.join(workspace, 'missing-work'), path)
        when :file
          File.write(path, "not a tracking directory\n")
        when :empty_directory
          FileUtils.mkdir_p(path)
        end

        tmux = ManagedTmux.new(slug, workspace:)
        runner = runner_for(workspace, tmux:)
        error = assert_raises(VpsfreeDevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.remove(slug, as_is: true, force: false)
          end
        end

        assert_match(/work directory|missing tracking files/, error.message)
        refute(tmux.killed)
      end
    end
  end

  def test_stop_rejects_an_archive_without_committed_active_history
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
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'terminal archive only')

      tmux = ManagedTmux.new(slug, workspace:)
      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/no committed active lifecycle/, error.message)
      refute(tmux.killed)
    end
  end

  def test_finalize_accepts_abandoned_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'abandoned')

      runner.finalize('demo', as_is: false)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_finalize_refuses_active_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/lifecycle is not terminal/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_missing_tracking_file
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.rm(File.join(workspace, 'work', slug, 'plan.md'))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/missing tracking files/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_tracking_without_a_prior_commit
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      set_lifecycle(workspace, slug, 'complete')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/tracking files have no prior commit/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_tracking_without_a_committed_active_state
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_terminal_tracking_only(workspace, slug, lifecycle: 'complete')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/no committed active lifecycle/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_uses_only_front_matter_for_current_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      state = File.join(workspace, 'work', slug, 'state.md')
      File.write(
        state,
        state_with_body_lifecycle(File.read(state), 'complete')
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize(slug, as_is: true)
      end

      assert_match(/not terminal: active/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_missing_lifecycle_front_matter
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      state = File.join(workspace, 'work', slug, 'state.md')
      content = File.read(state).sub(/\A---\nlifecycle: active\n---\n\n/, '')
      File.write(state, "#{content}\n- Lifecycle: complete\n")

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize(slug, as_is: true)
      end

      assert_match(/must start with lifecycle YAML front matter/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_lifecycle_front_matter_has_exact_boundaries
    with_workspace do |workspace|
      runner = runner_for(workspace)
      assert_nil(
        runner.send(
          :validate_terminal_lifecycle_content!,
          "---\r\nlifecycle: complete\r\n---\r\n\r\n# State\r\n"
        )
      )

      invalid = [
        " \n---\nlifecycle: complete\n---\n",
        "\uFEFF---\nlifecycle: complete\n---\n",
        "---\nlifecycle: complete\nowner: agent\n---\n",
        "---\nlifecycle: complete\n--- trailing\n"
      ]
      invalid.each do |content|
        error = assert_raises(VpsfreeDevSession::Error) do
          runner.send(:validate_terminal_lifecycle_content!, content)
        end
        assert_match(/must start with lifecycle YAML front matter/, error.message)
      end
    end
  end

  def test_ruby_lifecycle_parser_accepts_shared_valid_fixtures
    fixtures = Dir[File.expand_path('fixtures/lifecycle-valid-*.md', __dir__)]
    refute_empty(fixtures)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        lifecycle = runner_for(workspace).send(:lifecycle_state, File.binread(fixture))
        assert_includes(%w[active complete abandoned], lifecycle, File.basename(fixture))
      end
    end
  end

  def test_ruby_lifecycle_parser_rejects_shared_invalid_fixtures
    fixtures = Dir[File.expand_path('fixtures/lifecycle-invalid-*.md', __dir__)]
    refute_empty(fixtures)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        assert_raises(VpsfreeDevSession::Error, File.basename(fixture)) do
          runner_for(workspace).send(:lifecycle_state, File.binread(fixture))
        end
      end
    end
  end

  def test_lifecycle_parser_rejects_invalid_utf8_and_oversized_input
    with_workspace do |workspace|
      runner = runner_for(workspace)
      assert_raises(VpsfreeDevSession::Error) do
        runner.send(:lifecycle_state, "---\nlifecycle: active\n---\n\xff".b)
      end
      assert_raises(VpsfreeDevSession::Error) do
        runner.send(:lifecycle_state, "---\nlifecycle: active\n---\n" + ('x' * 1024 * 1024))
      end
    end
  end

  def test_finalize_uses_only_front_matter_for_active_history
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      state = File.join(workspace, 'work', slug, 'state.md')
      set_lifecycle(workspace, slug, 'complete')
      File.write(
        state,
        state_with_body_lifecycle(File.read(state), 'active')
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('work', slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'pseudo active state')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize(slug, as_is: true)
      end

      assert_match(/no committed active lifecycle/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_stop_uses_only_front_matter_for_archived_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      tmux = ManagedTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)
      runner.finalize(slug, as_is: true)
      state = File.join(workspace, 'archive', slug, 'state.md')
      content = File.read(state).sub('lifecycle: complete', 'lifecycle: active')
      File.write(state, state_with_body_lifecycle(content, 'complete'))
      commit_archive_move(workspace, slug)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.stop(slug, as_is: true)
      end

      assert_match(/not terminal: active/, error.message)
      refute(tmux.killed)
    end
  end

  def test_finalize_refuses_existing_archive
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.mkdir_p(File.join(workspace, 'archive', slug))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/archive already exists/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_dangling_archive_symlink
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      FileUtils.mkdir_p(File.join(workspace, 'archive'))
      FileUtils.ln_s(
        File.join(workspace, 'missing-archive-target'),
        File.join(workspace, 'archive', slug)
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/archive already exists/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_dirty_worktree_without_changing_session
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      commit_tracking(workspace, slug, lifecycle: 'complete')
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")
      tmux = ManagedTmux.new(slug, workspace:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).finalize('demo', as_is: false)
      end

      assert_match(/uncommitted changes/, error.message)
      refute(tmux.killed)
      assert(File.directory?(path))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_clean_detached_worktree_head
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      commit_tracking(workspace, slug, lifecycle: 'complete')
      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_git_success('git', '-C', path, 'switch', '--detach')
      configure_git_identity(path)
      File.write(File.join(path, 'detached.txt'), "unique commit\n")
      assert_git_success('git', '-C', path, 'add', 'detached.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'detached work')
      detached_head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/detached HEAD/, error.message)
      assert(File.directory?(path))
      assert_equal(detached_head, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip)
    end
  end

  def test_worktree_remove_refuses_a_per_worktree_symbolic_ref
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      path = File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
      assert_git_success('git', '-C', path, 'symbolic-ref', 'HEAD', 'refs/worktree/private-save')
      configure_git_identity(path)
      File.write(File.join(path, 'saved.txt'), "saved commit\n")
      assert_git_success('git', '-C', path, 'add', 'saved.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'private worktree commit')
      head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.worktree_remove('demo', 'sample', as_is: false, force: false)
      end

      assert_match(/retained shared branch/, error.message)
      assert(File.directory?(path))
      assert_git_success('git', '-C', path, 'cat-file', '-e', "#{head}^{commit}")
    end
  end

  def test_finalize_refuses_a_symlinked_worktree_entry
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      outside = File.join(workspace, 'outside-sample')
      bare = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git',
        "--git-dir=#{bare}",
        'worktree',
        'add',
        '-b',
        'outside',
        outside,
        'master'
      )
      FileUtils.ln_s(outside, File.join(workspace, 'worktrees', slug, 'sample'))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/unmanaged entries/, error.message)
      assert(File.directory?(outside))
      assert_git_success('git', '-C', outside, 'status', '--short')
    end
  end

  def test_finalize_refuses_a_symlinked_work_root
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      work_root = File.join(workspace, 'work')
      outside_root = File.join(workspace, 'outside-work')
      FileUtils.mv(work_root, outside_root)
      FileUtils.ln_s(outside_root, work_root)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/work root is a symlink/, error.message)
      assert(File.directory?(File.join(outside_root, slug)))
      assert(File.file?(File.join(outside_root, slug, 'state.md')))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_finalize_uses_an_atomic_no_clobber_archive_move
    skip 'git is not available' unless command_available?('git')

    %i[directory symlink].each do |collision|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        base_runner = runner_for(workspace)
        base_runner.ensure_tracking_files(slug)
        commit_tracking(workspace, slug, lifecycle: 'complete')
        source = File.join(workspace, 'work', slug)
        destination = File.join(workspace, 'archive', slug)

        command_runner = CallbackCommandRunner.new(
          out: StringIO.new,
          err: StringIO.new
        ) do |argv|
          next unless argv.first == 'mv' && argv.last != '--help'

          if collision == :directory
            FileUtils.mkdir_p(destination)
          else
            FileUtils.ln_s(File.join(workspace, 'collision-target'), destination)
          end
        end
        runner = VpsfreeDevSession::Runner.new(
          workspace:,
          command_runner:,
          tmux: NullTmux.new,
          out: StringIO.new,
          err: StringIO.new,
          today: TODAY
        )

        assert_raises(VpsfreeDevSession::Error) do
          runner.finalize('demo', as_is: false)
        end

        assert(File.directory?(source), "#{collision} collision moved the source")
        assert(File.file?(File.join(source, 'state.md')))
        if collision == :symlink
          assert(File.symlink?(destination))
        else
          assert_equal([], Dir.children(destination))
        end
      end
    end
  end

  def test_finalize_checks_atomic_move_support_before_worktree_cleanup
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      base_runner = runner_for(workspace)
      base_runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      commit_tracking(workspace, slug, lifecycle: 'complete')
      path = File.join(workspace, 'worktrees', slug, 'sample')
      command_runner = CallbackCommandRunner.new(
        out: StringIO.new,
        err: StringIO.new
      ) do |argv|
        next unless argv.first == 'mv' && argv.last == '--help'

        raise VpsfreeDevSession::Error, 'atomic move options unavailable'
      end
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        command_runner:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      assert_raises(VpsfreeDevSession::Error) do
        runner.finalize(slug, as_is: true)
      end

      assert(File.directory?(path))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_preflights_and_executes_one_archive_move_command
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      base_runner = runner_for(workspace)
      base_runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      move_commands = []
      command_runner = CallbackCommandRunner.new(
        out: StringIO.new,
        err: StringIO.new
      ) do |argv|
        move_commands << argv if argv.first == 'mv'
      end
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        command_runner:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.finalize(slug, as_is: true)

      assert_equal(2, move_commands.length)
      move_commands.each do |argv|
        assert_equal(
          ['mv', *VpsfreeDevSession::ARCHIVE_MOVE_OPTIONS],
          argv.take(VpsfreeDevSession::ARCHIVE_MOVE_OPTIONS.length + 1)
        )
      end
      assert_equal('--help', move_commands.first.last)
      assert_equal(File.join(workspace, 'archive', slug), move_commands.last.last)
    end
  end

  def test_dev_session_serializes_commands_per_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'
      other_slug = '2026-06-06-other'
      runner.ensure_tracking_files(slug)
      runner.ensure_tracking_files(other_slug)
      lock_root = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(lock_root)
      lock_path = File.join(lock_root, "#{slug}.lock")

      File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
        assert(lock.flock(File::LOCK_EX | File::LOCK_NB))

        error = assert_raises(VpsfreeDevSession::Error) do
          runner.remove(slug, as_is: true, force: false)
        end
        assert_match(/another dev-session command/, error.message)

        runner.remove(other_slug, as_is: true, force: false)
        refute(File.exist?(File.join(workspace, 'worktrees', other_slug)))
      end

      runner.remove(slug, as_is: true, force: false)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_finalize_refuses_unmanaged_worktree_group_entries
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug, 'cache'))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/contains unmanaged entries/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_registered_git_worktree_missing_from_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_git_success('git', "--git-dir=#{repository}", 'worktree', 'add', path, 'master')
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize(slug, as_is: true)
      end

      assert_match(/missing from the portal manifest/, error.message)
      assert(File.directory?(path))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_worktree_from_outside_canonical_repositories
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      Dir.mktmpdir('external-dev-session-repository') do |external|
        FileUtils.mkdir_p(File.join(external, 'repos'))
        create_bare_repo(external, 'sample')
        bare = File.join(external, 'repos', 'sample.git')
        path = File.join(workspace, 'worktrees', slug, 'external')
        assert_git_success('git', "--git-dir=#{bare}", 'worktree', 'add', path, 'master')

        error = assert_raises(VpsfreeDevSession::Error) do
          runner.finalize(slug, as_is: true)
        end

        assert_match(/outside the canonical repository root/, error.message)
        assert(File.directory?(path))
      end
    end
  end

  def test_finalize_refuses_unmanaged_tmux_session
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux: UnmanagedTmux.new(slug)).finalize(
          'demo',
          as_is: false
        )
      end

      assert_match(/not managed/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tmux_targets_do_not_prefix_match_a_longer_session
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'
    longer_slug = "#{slug}-long"

    with_workspace do |workspace|
      tmux_run(socket, 'new-session', '-d', '-s', longer_slug, '-c', workspace)
      command_runner = VpsfreeDevSession::CommandRunner.new(
        out: StringIO.new,
        err: StringIO.new
      )
      tmux = VpsfreeDevSession::Tmux.new(runner: command_runner, socket:)

      refute(tmux.session(slug))
      assert(tmux.session(longer_slug))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/session not found/, error.message)
      assert(tmux_session_exists?(socket, longer_slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_start_does_not_adopt_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ReplacedDuringCreateTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(VpsfreeDevSession::Error) do
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

      error = assert_raises(VpsfreeDevSession::Error) do
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
        error = assert_raises(VpsfreeDevSession::Error) do
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
      session = VpsfreeDevSession::Tmux::Session.new(
        id: '$partial',
        name: slug,
        mark: '',
        slug: '',
        workspace:,
        environment_slug: slug
      )

      runner.send(:reconcile_creation_tmux_session!, slug, session)

      assert_equal([['kill-session', '-t', '$partial:']], tmux.mutations)

      replacement = session.dup
      replacement.environment_slug = '2026-06-06-other'
      error = assert_raises(VpsfreeDevSession::Error) do
        runner.send(:reconcile_creation_tmux_session!, slug, replacement)
      end
      assert_includes(error.message, 'not recoverable')
      assert_equal(1, tmux.mutations.length)
    end
  end

  def test_worktree_mutations_use_the_slug_lock
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      lock_path = File.join(workspace, 'worktrees', '.locks', "#{slug}.lock")
      FileUtils.mkdir_p(File.dirname(lock_path))

      File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
        assert(lock.flock(File::LOCK_EX | File::LOCK_NB))

        add_error = assert_raises(VpsfreeDevSession::Error) do
          runner.worktree_add(
            slug,
            'sample',
            as_is: true,
            name: nil,
            branch: nil,
            base: nil,
            fetch: false
          )
        end
        assert_match(/another dev-session command/, add_error.message)

        remove_error = assert_raises(VpsfreeDevSession::Error) do
          runner.worktree_remove(slug, 'sample', as_is: true, force: false)
        end
        assert_match(/another dev-session command/, remove_error.message)
      end
    end
  end

  def test_stop_does_not_kill_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner_for(workspace).ensure_tracking_files(slug)
      tmux = ReplacedTmux.new(slug, workspace:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/session changed during operation/, error.message)
      refute(tmux.kill_attempted)
    end
  end

  def test_stop_refuses_an_active_codex_turn_before_killing_tmux_or_authority
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1',
        id: '$7'
      )
      failing_portal = [RbConfig.ruby, '-e', "warn 'thread is active'; exit 1"]
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        tmux:,
        portal_command: failing_portal,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      assert_raises(VpsfreeDevSession::CommandError) do
        runner.stop(slug, as_is: true)
      end
      refute(tmux.killed)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))

      idle_runner = VpsfreeDevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        tmux:,
        portal_command: ['true'],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      idle_runner.stop(slug, as_is: true)
      assert(tmux.killed)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_stop_quiesces_terminal_before_the_authoritative_idle_check
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      active = File.join(workspace, 'turn-active')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~RUBY)
        #!/usr/bin/env ruby
        abort 'turn became active while terminal was quiesced' if File.exist?(#{active.dump})
      RUBY
      File.chmod(0o755, portal)
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        on_kill: -> { File.write(active, "active\n") },
        socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1',
        id: '$7'
      )
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        tmux:,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      error = assert_raises(VpsfreeDevSession::CommandError) do
        runner.stop(slug, as_is: true)
      end
      assert_includes(error.message, 'turn became active')
      refute(tmux.killed)
      refute(tmux.quiesced, 'terminal Codex client was not restored')
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_recover_stale_removes_only_idle_identity_validated_authority
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1',
        id: '$7'
      )
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        tmux:,
        portal_command: ['true'],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      assert_raises(VpsfreeDevSession::Error) do
        runner.recover_stale(slug, as_is: true)
      end

      tmux.run('kill-session', '-t', '$7:')
      runner.recover_stale(slug, as_is: true)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_remove_does_not_kill_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner_for(workspace).ensure_tracking_files(slug)
      tmux = ReplacedTmux.new(slug, workspace:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).remove(slug, as_is: true, force: false)
      end

      assert_match(/session changed during operation/, error.message)
      refute(tmux.kill_attempted)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_managed_tmux_session_is_bound_to_its_workspace
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )
      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      pane = tmux_capture(socket, 'list-panes', '-t', slug, '-F', '#{pane_id}')
             .lines
             .first
             .strip

      Dir.mktmpdir('other-dev-session-workspace') do |other_workspace|
        other_out = StringIO.new
        other_runner = VpsfreeDevSession::Runner.new(
          workspace: other_workspace,
          tmux_socket: socket,
          out: other_out,
          err: StringIO.new,
          today: TODAY,
          env: { 'TMUX' => 'socket', 'TMUX_PANE' => pane }
        )

        current_error = assert_raises(VpsfreeDevSession::Error) do
          other_runner.current
        end
        assert_match(/not managed by this workspace/, current_error.message)

        other_runner.list
        assert_equal('', other_out.string)
        lookup_error = assert_raises(VpsfreeDevSession::Error) do
          other_runner.lookup_slug('demo', as_is: false)
        end
        assert_match(/no slug found/, lookup_error.message)

        error = assert_raises(VpsfreeDevSession::Error) do
          other_runner.stop(slug, as_is: true)
        end

        assert_match(/not managed by this workspace/, error.message)
      end

      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_finalize_runtime_output_uses_deployed_helper
    slug = '2026-06-06-demo'
    with_workspace do |workspace|
      runner_for(workspace).send(:ensure_tracking_files, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      out = StringIO.new
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        tmux_socket: '/run/workspace-tmux/tmux.sock',
        authority_dir: File.join(workspace, 'runtime-authority'),
        codex_socket: '/run/workspace-codex/app-server.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        portal_command: ['/run/current-system/sw/bin/workspace-portal'],
        require_runtime: true,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      runner.finalize(slug, as_is: true)

      assert_includes(
        out.string,
        "stop after committing: dev-session stop #{slug} --as-is"
      )
    end
  end

  def test_finalize_keeps_real_tmux_session_until_explicit_stop
    skip 'git is not available' unless command_available?('git')
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      out = StringIO.new
      authority_dir = File.join(workspace, 'runtime-authority')
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        authority_dir:,
        codex_command: 'false',
        out:,
        err: StringIO.new,
        today: TODAY
      )
      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      ordinary_runner = VpsfreeDevSession::Runner.new(
        workspace:,
        authority_dir:,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      ordinary_runner.finalize('demo', as_is: false)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
      assert(tmux_session_exists?(socket, slug))
      assert_includes(out.string, 'stop after committing')

      commit_archive_move(workspace, slug)
      ordinary_runner.stop(slug, as_is: true)
      refute(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_tmux_codex_runs_from_shell_and_leaves_shell_available
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      codex_probe = File.join(workspace, 'codex-ran.txt')
      shell_probe = File.join(workspace, 'shell-remained.txt')
      codex_command = "printf codex > #{Shellwords.escape(codex_probe)}"
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command:,
        portal_command: [],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: true)
      wait_for_file(codex_probe)

      panes = tmux_capture(
        socket,
        'list-panes',
        '-t',
        "#{slug}:dev",
        '-F',
        "\#{pane_id}\t\#{pane_current_path}"
      ).lines.map { |line| line.chomp.split("\t", 2) }
      left = panes.find { |_id, path| path == workspace }.fetch(0)
      command = "printf shell > #{Shellwords.escape(shell_probe)}"

      tmux_run(socket, 'send-keys', '-t', left, '-l', command)
      tmux_run(socket, 'send-keys', '-t', left, 'Enter')
      wait_for_file(shell_probe)

      assert_equal('codex', File.read(codex_probe))
      assert_equal('shell', File.read(shell_probe))
      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_shared_session_records_codex_endpoint_provenance
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'
    codex_socket = '/run/vpsfree-workspace-codex/app-server.sock'

    with_workspace do |workspace|
      codex_executable = File.join(workspace, 'codex')
      File.write(codex_executable, "#!/bin/sh\n[ \"$1\" = --version ] && { echo 'codex-cli 0.152.1'; exit 0; }\nexit 0\n")
      File.chmod(0o755, codex_executable)
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_socket:,
        codex_version: '0.152.1',
        codex_command: codex_executable,
        portal_command: [
          RbConfig.ruby,
          '-e',
          "require 'json'; puts JSON.generate(threadId: 'thread-123')"
        ],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: true)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal(codex_socket, manifest.dig('codex', 'socket_path'))
      assert_equal('0.152.1', manifest.dig('codex', 'client_version'))
      refute(manifest.key?('tmux'))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_codex_provenance_requires_the_configured_executable_version
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      codex_executable = File.join(workspace, 'codex')
      File.write(codex_executable, "#!/bin/sh\necho 'codex-cli 0.151.0'\n")
      File.chmod(0o755, codex_executable)
      session = VpsfreeDevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:
      )
      runner_class = Class.new(VpsfreeDevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **_keywords|
          session
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex_executable,
        portal_command: [
          RbConfig.ruby,
          '-e',
          "require 'json'; puts JSON.generate(threadId: 'thread-123')"
        ],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: true)
      end
      assert_includes(error.message, 'does not report configured version')
    end
  end

  def test_tmux_start_and_sync_manage_only_worktree_windows
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      worktree = File.join(workspace, 'worktrees', slug, 'alpha')
      FileUtils.mkdir_p(worktree)
      File.write(File.join(worktree, '.git'), "gitdir: /tmp/nonexistent\n")

      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)

      session_env = tmux_capture(socket, 'show-environment', '-t', slug)
      assert_includes(session_env, "#{VpsfreeDevSession::ENV_SLUG}=#{slug}\n")
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_WORKSPACE}=#{workspace}\n"
      )
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_WORK_DIR}=#{File.join(workspace, 'work', slug)}\n"
      )
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_WORKTREES_DIR}=#{File.join(workspace, 'worktrees', slug)}\n"
      )
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_PORTAL_BASE_URL}=#{VpsfreeDevSession::DEFAULT_PORTAL_BASE_URL}\n"
      )
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_PORTAL_URL}=#{VpsfreeDevSession::DEFAULT_PORTAL_BASE_URL}/#{slug}/\n"
      )

      panes = tmux_capture(socket, 'list-panes', '-t', "#{slug}:dev", '-F', '#{pane_current_path}')
              .lines
              .map(&:chomp)

      assert_equal(3, panes.length)
      assert_includes(panes, workspace)
      assert_includes(panes, File.join(workspace, 'work', slug))
      assert_includes(panes, File.join(workspace, 'worktrees', slug))

      windows = tmux_capture(
        socket,
        'list-windows',
        '-t',
        slug,
        '-F',
        '#{window_name}:#{@vpsfree_dev_session_window}'
      ).lines.map(&:chomp)

      assert_includes(windows, 'alpha:worktree')

      probe = File.join(workspace, 'alpha-env.txt')
      command = "printf '%s\\n' \"$#{VpsfreeDevSession::ENV_SLUG}\" > #{Shellwords.escape(probe)}"
      tmux_run(socket, 'send-keys', '-t', "#{slug}:alpha", command, 'Enter')
      wait_for_file(probe)
      assert_equal(slug, File.read(probe).strip)

      tmux_run(socket, 'new-window', '-d', '-t', slug, '-n', 'custom', '-c', workspace)
      FileUtils.rm_rf(worktree)
      runner.sync('demo', as_is: false)

      names = tmux_capture(socket, 'list-windows', '-t', slug, '-F', '#{window_name}')
              .lines
              .map(&:chomp)

      assert_includes(names, 'custom')
      refute_includes(names, 'alpha')
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_codex_endpoint_identity_survives_compatible_client_upgrade
    with_workspace do |workspace|
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        out: StringIO.new,
        err: StringIO.new,
        env: {}
      )
      session = VpsfreeDevSession::Tmux::Session.new(
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.152.1'
      )

      assert(runner.send(:session_codex_provenance_matches?, session, 'thread-1'))
      session.codex_socket_path = '/run/workspace/other.sock'
      refute(runner.send(:session_codex_provenance_matches?, session, 'thread-1'))
    end
  end

  def test_attach_restarts_terminal_client_after_app_server_disconnect
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.152.1',
        pane_current_command: 'sh'
      )
      errors = StringIO.new
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux:,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        out: StringIO.new,
        err: errors,
        env: { 'SHELL' => '/bin/sh' }
      )

      session = runner.send(:reconcile_native_client!, slug, tmux.session(slug))

      assert_equal('$managed', session.id)
      assert_equal('0.153.2', session.codex_client_version)
      assert_equal(1, tmux.sent_commands.length)
      assert_includes(tmux.sent_commands.first, codex)
      assert_includes(tmux.sent_commands.first, '--remote unix:///run/workspace/codex.sock')
      assert_includes(tmux.sent_commands.first, 'resume thread-1')
      assert_includes(errors.string, 'restarted terminal Codex client')

      running = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2',
        pane_current_command: '.codex-wrapped'
      )
      running_runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: running,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )
      running_runner.send(:reconcile_native_client!, slug, running.session(slug))
      assert_empty(running.sent_commands)
    end
  end

  private

  def with_workspace
    Dir.mktmpdir('dev-session-test') do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'repos'))
      FileUtils.mkdir_p(File.join(workspace, 'work'))
      FileUtils.mkdir_p(File.join(workspace, 'worktrees'))
      yield workspace
    end
  end

  def runner_for(workspace, env: {}, cwd: nil, tmux: nil, out: nil)
    out ||= StringIO.new
    tmux ||= NullTmux.new

    VpsfreeDevSession::Runner.new(
      workspace:,
      tmux:,
      out:,
      err: StringIO.new,
      today: TODAY,
      env:,
      cwd: cwd || workspace
    )
  end

  def create_bare_repo(workspace, project)
    source = File.join(workspace, "source-#{project}")
    bare = File.join(workspace, 'repos', "#{project}.git")

    assert_git_success('git', 'init', '-b', 'master', source)
    assert_git_success('git', '-C', source, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', source, 'config', 'user.name', 'Test User')
    File.write(File.join(source, 'README.md'), "# Test\n")
    assert_git_success('git', '-C', source, 'add', 'README.md')
    assert_git_success('git', '-C', source, 'commit', '-m', 'initial')
    assert_git_success('git', 'clone', '--bare', source, bare)
  end

  def commit_tracking(workspace, slug, lifecycle:)
    assert_git_success('git', 'init', '-b', 'master', workspace)
    assert_git_success('git', '-C', workspace, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', workspace, 'config', 'user.name', 'Test User')
    assert_git_success('git', '-C', workspace, 'add', File.join('work', slug))
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'start initiative')
    return if lifecycle == 'active'

    set_lifecycle(workspace, slug, lifecycle)
    assert_git_success('git', '-C', workspace, 'add', File.join('work', slug, 'state.md'))
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'close initiative')
  end

  def commit_terminal_tracking_only(workspace, slug, lifecycle:)
    set_lifecycle(workspace, slug, lifecycle)
    assert_git_success('git', 'init', '-b', 'master', workspace)
    configure_git_identity(workspace)
    assert_git_success('git', '-C', workspace, 'add', File.join('work', slug))
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'terminal initiative')
  end

  def commit_archive_move(workspace, slug)
    assert_git_success(
      'git',
      '-C',
      workspace,
      'add',
      '-A',
      '--',
      File.join('work', slug),
      File.join('archive', slug)
    )
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'archive initiative')
  end

  def set_lifecycle(workspace, slug, lifecycle)
    state = File.join(workspace, 'work', slug, 'state.md')
    content = File.read(state).sub(
      /\A---\nlifecycle: (?:active|complete|abandoned)\n---/,
      "---\nlifecycle: #{lifecycle}\n---"
    )
    File.write(state, content)
  end

  def state_with_body_lifecycle(content, lifecycle)
    fragment = "# Appendix\n\n- Lifecycle: #{lifecycle}\n\n"
    content.sub("## Results\n", "#{fragment}## Results\n")
  end

  def assert_git_success(*argv)
    stdout, stderr, status = Open3.capture3(*argv)
    assert(status.success?, "command failed: #{argv.join(' ')}\n#{stdout}\n#{stderr}")
  end

  def refute_git_success(*argv)
    stdout, stderr, status = Open3.capture3(*argv)
    message = "command unexpectedly succeeded: #{argv.join(' ')}\n#{stdout}\n#{stderr}"
    refute(status.success?, message)
  end

  def git_capture_success(*argv)
    stdout, stderr, status = Open3.capture3(*argv)
    assert(status.success?, "command failed: #{argv.join(' ')}\n#{stdout}\n#{stderr}")
    stdout
  end

  def configure_git_identity(path)
    assert_git_success('git', '-C', path, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', path, 'config', 'user.name', 'Test User')
  end

  def tmux_capture(socket, *args)
    stdout, stderr, status = Open3.capture3('tmux', '-L', socket, *args)
    assert(status.success?, "tmux failed: #{args.join(' ')}\n#{stdout}\n#{stderr}")
    stdout
  end

  def tmux_run(socket, *args, allow_failure: false)
    _stdout, _stderr, status = Open3.capture3('tmux', '-L', socket, *args)
    assert(status.success?, "tmux failed: #{args.join(' ')}") unless allow_failure
    status
  end

  def tmux_session_exists?(socket, slug)
    target = VpsfreeDevSession::Tmux.session_target(slug)
    _stdout, _stderr, status = Open3.capture3('tmux', '-L', socket, 'has-session', '-t', target)
    status.success?
  end

  def wait_for_file(path)
    deadline = Time.now + 5

    until File.exist?(path)
      raise "timed out waiting for #{path}" if Time.now > deadline

      sleep 0.05
    end
  end

  def command_available?(cmd)
    ENV.fetch('PATH', '').split(File::PATH_SEPARATOR).any? do |dir|
      File.executable?(File.join(dir, cmd))
    end
  end

  def tmux_test_available?
    return false if ENV['VPSFREE_DEV_SESSION_SKIP_REAL_TMUX_TESTS'] == '1'
    return @tmux_test_available unless @tmux_test_available.nil?
    return @tmux_test_available = false unless command_available?('tmux')

    socket = "dev-session-probe-#{Process.pid}-#{object_id}"
    shell = ENV.fetch('SHELL', '/bin/sh')
    _stdout, _stderr, status = Open3.capture3(
      'tmux', '-L', socket, 'new-session', '-d', '-s', 'probe', shell
    )
    _stdout, _stderr, live_status = Open3.capture3(
      'tmux', '-L', socket, 'has-session', '-t', '=probe:'
    )
    @tmux_test_available = status.success? && live_status.success?
    Open3.capture3('tmux', '-L', socket, 'kill-server') if @tmux_test_available
    @tmux_test_available
  end
end
