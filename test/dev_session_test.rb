# frozen_string_literal: true

require 'date'
require 'fileutils'
require 'json'
require 'minitest/autorun'
require 'open3'
require 'pty'
require 'rbconfig'
require 'shellwords'
require 'stringio'
require 'tmpdir'

load File.expand_path('../libexec/dev-session', __dir__)

class DevSessionTest < Minitest::Test
  class TTYInput < StringIO
    def tty?
      true
    end
  end

  class SignalingTTYInput < TTYInput
    def initialize(value, read:)
      super(value)
      @read = read
    end

    def gets(*arguments)
      value = super
      @read << true
      value
    end
  end

  class LockingCLI < DevSession::CLI
    def initialize(*args, entered:, release:, **options)
      super(*args, **options)
      @entered = entered
      @release = release
    end

    private

    def run_command(_command)
      @entered << true
      @release.pop
    end
  end

  def test_cli_holds_the_shared_host_transition_lock_while_mutating_state
    Dir.mktmpdir('dev-session-transition-lock-test') do |directory|
      path = File.join(directory, 'transition.lock')
      entered = Queue.new
      release = Queue.new
      cli = LockingCLI.new(
        ['--transition-lock', path, 'probe'],
        entered:,
        release:,
        out: StringIO.new,
        err: StringIO.new
      )
      result = nil
      thread = Thread.new { result = cli.run }
      entered.pop

      File.open(path, File::RDWR) do |file|
        refute(file.flock(File::LOCK_EX | File::LOCK_NB))
        release << true
        thread.join
        assert(file.flock(File::LOCK_EX | File::LOCK_NB))
      end
      assert_equal(0, result)
    ensure
      release << true if thread&.alive?
      thread&.join
    end
  end

  def test_cli_confirms_lifecycle_action_before_waiting_for_the_transition_lock
    Dir.mktmpdir('dev-session-lifecycle-lock-test') do |directory|
      path = File.join(directory, 'transition.lock')
      owner = File.open(path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      prompt_read = Queue.new
      calls = []
      fake_runner = Object.new
      fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
      fake_runner.define_singleton_method(:delete) do |input, as_is:, force:, operation_id:|
        calls << [input, as_is, force, operation_id]
      end
      cli = DevSession::CLI.new(
        [
          '--transition-lock', path, '--', 'delete',
          '2026-06-06-demo', '--as-is'
        ],
        input: SignalingTTYInput.new("yes\n", read: prompt_read),
        out: StringIO.new,
        err: StringIO.new
      )
      cli.define_singleton_method(:runner) { fake_runner }

      thread = Thread.new { cli.run }
      prompt_read.pop
      sleep 0.05
      assert_empty(calls)
      owner.flock(File::LOCK_UN)

      assert_equal(0, thread.value)
      assert_equal([['2026-06-06-demo', true, false, nil]], calls)
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
      thread&.join
    end
  end

  def test_lifecycle_confirmation_uses_a_real_terminal_before_taking_the_lock
    Dir.mktmpdir('dev-session-lifecycle-pty-test') do |directory|
      path = File.join(directory, 'transition.lock')
      marker = File.join(directory, 'deleted')
      owner = File.open(path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      master, slave = PTY.open
      pid = fork do
        master.close
        $stdin.reopen(slave)
        $stdout.reopen(slave)
        $stderr.reopen(slave)
        slave.close
        fake_runner = Object.new
        fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
        fake_runner.define_singleton_method(:delete) do |_input, as_is:, force:, operation_id:|
          File.write(marker, "#{as_is}:#{force}:#{operation_id.inspect}\n")
        end
        cli = DevSession::CLI.new([
          '--transition-lock', path, '--', 'delete',
          '2026-06-06-demo', '--as-is'
        ])
        cli.define_singleton_method(:runner) { fake_runner }
        exit!(cli.run)
      end
      slave.close

      prompt = +''
      until prompt.include?('Delete session 2026-06-06-demo? [y/N]')
        ready = IO.select([master], nil, nil, 2)
        flunk('timed out waiting for lifecycle confirmation prompt') unless ready
        prompt << master.read_nonblock(4096)
      end
      master.write("y\n")
      sleep 0.05
      refute(File.exist?(marker))
      assert(Process.kill(0, pid))
      owner.flock(File::LOCK_UN)

      _waited, status = Process.wait2(pid)
      pid = nil
      assert(status.success?)
      assert_equal("true:false:nil\n", File.read(marker))
    ensure
      if pid
        Process.kill('TERM', pid) rescue nil
        Process.wait(pid) rescue nil
      end
      master&.close unless master&.closed?
      slave&.close unless slave&.closed?
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_waiting_lifecycle_cli_rejects_a_generation_changed_after_confirmation
    %w[successful-switch compensated-switch].each do |scenario|
      Dir.mktmpdir("dev-session-#{scenario}") do |directory|
        path = File.join(directory, 'transition.lock')
        expected = File.join(directory, 'old')
        selected = File.join(directory, 'new')
        profile = File.join(directory, 'profile')
        FileUtils.mkdir_p(expected)
        FileUtils.mkdir_p(selected)
        File.symlink(expected, profile)
        owner = File.open(path, File::RDWR | File::CREAT, 0o600)
        owner.flock(File::LOCK_EX)
        prompt_read = Queue.new
        calls = []
        error_output = StringIO.new
        fake_runner = Object.new
        fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
        fake_runner.define_singleton_method(:delete) { |*arguments, **options| calls << [arguments, options] }
        cli = DevSession::CLI.new(
          [
            '--transition-lock', path,
            '--host-profile', profile,
            '--expected-host-generation', expected,
            '--expected-host-profile-token', profile_link_token(profile),
            '--', 'delete', '2026-06-06-demo', '--as-is'
          ],
          input: SignalingTTYInput.new("yes\n", read: prompt_read),
          out: StringIO.new,
          err: error_output
        )
        cli.define_singleton_method(:runner) { fake_runner }

        thread = Thread.new { cli.run }
        prompt_read.pop
        File.unlink(profile)
        File.symlink(selected, profile)
        if scenario == 'compensated-switch'
          File.unlink(profile)
          File.symlink(expected, profile)
        end
        owner.flock(File::LOCK_UN)

        assert_equal(1, thread.value)
        assert_empty(calls)
        assert_includes(error_output.string, 'superseded package generation')
      ensure
        owner&.flock(File::LOCK_UN)
        owner&.close
        thread&.join
      end
    end
  end

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
      DevSession::Tmux::Session.new(
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
      identity_token: 'a' * 64,
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
      @identity_token = identity_token
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
      if args.first == 'set-option' && args[-2] == DevSession::SESSION_CODEX_VERSION
        @codex_client_version = args.last
        return
      end
      if args.first == 'set-environment' && args[-2] == DevSession::ENV_TMUX_IDENTITY
        @identity_token = args.last
        return
      end
      targets = ["#{@id}:", @slug]
      return unless args.first(2) == ['kill-session', '-t'] && targets.include?(args[2])

      @on_kill&.call
      @killed = true
    end

    def kill_session_if_identity(id, identity_token)
      return false unless id == @id && identity_token == @identity_token && !@killed

      run('kill-session', '-t', "#{id}:")
      true
    end

    def initialize_session_identity(id, identity_token)
      return false unless id == @id && !@killed &&
                          (@identity_token.nil? || @identity_token.empty?)

      @identity_token = identity_token
      true
    end

    private

    def identity
      DevSession::Tmux::Session.new(
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
        codex_pane_id: @codex_pane_id,
        identity_token: @identity_token
      )
    end
  end

  class WindowRecordingTmux < ManagedTmux
    attr_reader :captures

    def initialize(*args, **options)
      super
      @captures = []
    end

    def capture(*args)
      @captures << args
      ['%new']
    end
  end

  class RenamedManagedTmux < ManagedTmux
    def session(_slug)
      nil
    end

    private

    def identity
      super.tap { |session| session.name = "#{@slug}-renamed" }
    end
  end

  class PartialManagedTmux < ManagedTmux
    private

    def identity
      super.tap do |session|
        session.mark = ''
        session.slug = ''
      end
    end
  end

  class KillThenFailOnceTmux < ManagedTmux
    def run(*args)
      killing = args.first(2) == ['kill-session', '-t']
      super
      return unless killing && !@reported_failure

      @reported_failure = true
      raise DevSession::Error, 'simulated failure after tmux removal'
    end
  end

  class MismatchedIdentityAfterKillTmux < ManagedTmux
    def session_by_id(id)
      return super unless @killed && id == @id

      DevSession::Tmux::Session.new(
        id: '$12',
        name: @slug,
        mark: '1',
        slug: @slug,
        workspace: @workspace,
        environment_slug: @slug,
        socket_path: @socket_path
      )
    end
  end

  class ReplacedDuringConditionalKillTmux < ManagedTmux
    attr_reader :conditional_kill_attempted

    def kill_session_if_identity(_id, _identity_token)
      @conditional_kill_attempted = true
      false
    end
  end

  class RefusedIdentityInitializationTmux < ManagedTmux
    attr_reader :identity_initialization_attempted

    def initialize_session_identity(_id, _identity_token)
      @identity_initialization_attempted = true
      false
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
         args[-2] == DevSession::ENV_WORKSPACE
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

      DevSession::Tmux::Session.new(
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

      DevSession::Tmux::Session.new(
        id: '$12',
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

      DevSession::Tmux::Session.new(
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
      @identity_token = nil
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
                 identity = args.find do |value|
                   value.start_with?("#{DevSession::ENV_TMUX_IDENTITY}=")
                 end
                 @identity_token = identity&.partition('=')&.last
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
      if args.first == 'set-environment' && args[-2] == DevSession::ENV_TMUX_IDENTITY
        @identity_token = args.last
      end
    end

    def windows(_session)
      []
    end

    private

    def identity(id, managed:)
      DevSession::Tmux::Session.new(
        id:,
        name: @slug,
        mark: managed ? '1' : '',
        slug: managed ? @slug : '',
        workspace: @workspace,
        identity_token: @identity_token
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
      @identity_token = nil
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
        identity = args.find { |value| value.start_with?("#{DevSession::ENV_TMUX_IDENTITY}=") }
        @identity_token = identity&.partition('=')&.last
        ["$partial\n", '', nil]
      when 'display-message'
        ["%left\n", '', nil]
      when 'split-window'
        @split_attempts += 1
        raise DevSession::Error, 'split failed'
      else
        raise "unexpected tmux capture: #{args.inspect}"
      end
    end

    def run(*args)
      if args.first == 'set-option' && args[-2] == DevSession::SESSION_MARK
        @mark = args.last
      elsif args.first == 'set-option' && args[-2] == DevSession::SESSION_SLUG
        @session_slug = args.last
      elsif args.first == 'set-environment' && args[-2] == DevSession::ENV_TMUX_IDENTITY
        @identity_token = args.last
      elsif args.first == 'kill-session'
        @created = false
        @kill_count += 1
      end
    end

    def kill_session_if_identity(id, identity_token)
      return false unless id == '$partial' && identity_token == @identity_token && @created

      run('kill-session', '-t', "#{id}:")
      true
    end

    private

    def identity
      DevSession::Tmux::Session.new(
        id: '$partial',
        name: @slug,
        mark: @mark,
        slug: @session_slug,
        workspace: @workspace,
        environment_slug: @slug,
        identity_token: @identity_token
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

    def kill_session_if_identity(id, identity_token)
      @mutations << ['conditional-kill-session', id, identity_token]
      true
    end
  end

  class CallbackCommandRunner
    def initialize(out:, err:, &callback)
      @delegate = DevSession::CommandRunner.new(out:, err:)
      @callback = callback
    end

    def capture(argv, allow_failure: false, **options)
      @callback.call(argv)
      @delegate.capture(argv, allow_failure:, **options)
    end

    def run(argv, **options)
      @callback.call(argv)
      @delegate.run(argv, **options)
    end
  end

  TODAY = Date.new(2026, 6, 6)

  def test_cli_prompts_for_a_new_session_initial_request
    captured = nil
    fake_runner = Object.new
    fake_runner.define_singleton_method(:start_requires_initial_goal?) do |_input, **_options|
      true
    end
    fake_runner.define_singleton_method(:start) do |input, **options|
      captured = {
        input:,
        options: options.dup,
        goal: options.fetch(:goal_text)
      }
    end
    err = StringIO.new
    cli = DevSession::CLI.new(
      ['start', 'demo', '--no-attach'],
      input: TTYInput.new("Investigate the API failure.\n"),
      out: StringIO.new,
      err:
    )
    cli.define_singleton_method(:runner) { fake_runner }

    assert_equal(0, cli.run)
    assert_equal('demo', captured.fetch(:input))
    assert_equal('Investigate the API failure.', captured.fetch(:goal))
    assert_includes(err.string, 'Initial request: ')
    assert_nil(captured.dig(:options, :goal_file))
  end

  def test_cli_requires_a_goal_file_for_noninteractive_new_session
    fake_runner = Object.new
    fake_runner.define_singleton_method(:start_requires_initial_goal?) do |_input, **_options|
      true
    end
    fake_runner.define_singleton_method(:start) { raise 'must not start' }
    err = StringIO.new
    cli = DevSession::CLI.new(
      ['start', 'demo', '--no-attach'],
      input: StringIO.new("ignored\n"),
      out: StringIO.new,
      err:
    )
    cli.define_singleton_method(:runner) { fake_runner }

    assert_equal(1, cli.run)
    assert_includes(err.string, 'requires --goal-file when input is not interactive')
  end

  def test_retired_lifecycle_commands_are_not_public
    %w[finalize remove reopen _finalize-url].each do |command|
      err = StringIO.new
      cli = DevSession::CLI.new(
        [command, '2026-06-06-demo', '--as-is'],
        input: StringIO.new,
        out: StringIO.new,
        err:
      )
      assert_equal(1, cli.run)
      assert_includes(err.string, "unknown command: #{command}")
    end
  end

  def test_archive_requires_a_simple_interactive_confirmation
    calls = []
    fake_runner = Object.new
    fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
    fake_runner.define_singleton_method(:archive) { |input, **options| calls << [input, options] }
    err = StringIO.new
    cli = DevSession::CLI.new(
      ['archive', '2026-06-06-demo', '--as-is'],
      input: StringIO.new,
      out: StringIO.new,
      err:
    )
    cli.define_singleton_method(:runner) { fake_runner }

    assert_equal(1, cli.run)
    assert_empty(calls)
    assert_includes(err.string, 'requires an interactive terminal')

    err = StringIO.new
    cli = DevSession::CLI.new(
      ['archive', '2026-06-06-demo', '--as-is'],
      input: TTYInput.new("no\n"),
      out: StringIO.new,
      err:
    )
    cli.define_singleton_method(:runner) { fake_runner }
    assert_equal(1, cli.run)
    assert_empty(calls)
    assert_includes(err.string, 'was not confirmed')

    cli = DevSession::CLI.new(
      ['archive', '2026-06-06-demo', '--as-is'],
      input: TTYInput.new("yes\n"),
      out: StringIO.new,
      err: StringIO.new
    )
    cli.define_singleton_method(:runner) { fake_runner }
    assert_equal(0, cli.run)
    assert_equal(
      [[
        '2026-06-06-demo',
        { as_is: true, abandoned: false, operation_id: nil }
      ]],
      calls
    )
  end

  def test_revive_confirmation_uses_the_archived_lifecycle
    calls = []
    fake_runner = Object.new
    fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
    fake_runner.define_singleton_method(:revive_confirmation) do |_input, as_is:|
      { lifecycle: 'abandoned', pending: false } if as_is
    end
    fake_runner.define_singleton_method(:revive) do |input, **options|
      calls << [input, options]
    end
    err = StringIO.new
    cli = DevSession::CLI.new(
      ['revive', '2026-06-06-demo', '--as-is'],
      input: TTYInput.new("yes\n"), out: StringIO.new, err:
    )
    cli.define_singleton_method(:runner) { fake_runner }

    assert_equal(0, cli.run)
    assert_includes(err.string, 'explicitly abandoned')
    assert_equal(
      [[
        '2026-06-06-demo',
        { as_is: true, allow_abandoned: true, operation_id: nil }
      ]],
      calls
    )
  end

  def test_revive_retry_uses_the_journaled_confirmation_without_another_prompt
    calls = []
    fake_runner = Object.new
    fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
    fake_runner.define_singleton_method(:revive_confirmation) do |_input, as_is:|
      { lifecycle: 'abandoned', pending: true } if as_is
    end
    fake_runner.define_singleton_method(:revive) do |input, **options|
      calls << [input, options]
    end
    cli = DevSession::CLI.new(
      ['revive', '2026-06-06-demo', '--as-is'],
      input: StringIO.new, out: StringIO.new, err: StringIO.new
    )
    cli.define_singleton_method(:runner) { fake_runner }

    assert_equal(0, cli.run)
    assert_equal(
      [[
        '2026-06-06-demo',
        { as_is: true, allow_abandoned: true, operation_id: nil }
      ]],
      calls
    )
  end

  def test_stop_requires_an_interactive_exact_slug_confirmation
    stopped = []
    fake_runner = Object.new
    fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
    fake_runner.define_singleton_method(:stop) { |input, as_is:| stopped << [input, as_is] }
    err = StringIO.new
    rejected = DevSession::CLI.new(
      ['stop', '2026-06-06-demo', '--as-is'],
      input: StringIO.new,
      out: StringIO.new,
      err:
    )
    rejected.define_singleton_method(:runner) { fake_runner }

    assert_equal(1, rejected.run)
    assert_empty(stopped)
    assert_includes(err.string, 'requires an interactive terminal')

    cli = DevSession::CLI.new(
      ['stop', '2026-06-06-demo', '--as-is'],
      input: TTYInput.new("2026-06-06-demo\n"),
      out: StringIO.new,
      err: StringIO.new
    )
    cli.define_singleton_method(:runner) { fake_runner }

    assert_equal(0, cli.run)
    assert_equal([['2026-06-06-demo', true]], stopped)
  end

  def test_portal_lifecycle_authorization_is_bound_to_its_service_cgroup
    Dir.mktmpdir('dev-session-cgroup-test') do |directory|
      cgroup = File.join(directory, 'cgroup')
      File.write(
        cgroup,
        "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" \
        "workspace-portal@example-workspace.service\n"
      )
      calls = []
      fake_runner = Object.new
      fake_runner.define_singleton_method(:resolve_slug) { |input, as_is:| input if as_is }
      fake_runner.define_singleton_method(:archive) { |input, **options| calls << [input, options] }
      fake_runner.define_singleton_method(:revive) { |input, **options| calls << [input, options] }
      fake_runner.define_singleton_method(:delete) do |input, as_is:, force:, operation_id:|
        calls << [input, { as_is:, force:, operation_id: }]
      end
      arguments = [
        '--require-runtime',
        '--authority-dir', '/run/user/1000/dev-workspaces/example-workspace/authority',
        '--portal-command', '/nix/store/portal/bin/workspace-portal',
        '--', 'archive', '2026-06-06-demo', '--as-is', '--portal-authorized',
        '--portal-operation-id', 'a' * 64
      ]
      cli = DevSession::CLI.new(
        arguments,
        input: StringIO.new,
        out: StringIO.new,
        err: StringIO.new,
        cgroup_file: cgroup,
        env: {}
      )
      cli.define_singleton_method(:runner) { fake_runner }

      assert_equal(0, cli.run)
      assert_equal(
        [[
          '2026-06-06-demo',
          { as_is: true, abandoned: false, operation_id: 'a' * 64 }
        ]],
        calls
      )

      missing_id = DevSession::CLI.new(
        arguments.first(arguments.length - 2),
        input: StringIO.new,
        out: StringIO.new,
        err: (missing_id_error = StringIO.new),
        cgroup_file: cgroup,
        env: {}
      )
      missing_id.define_singleton_method(:runner) { fake_runner }
      assert_equal(1, missing_id.run)
      assert_includes(missing_id_error.string, 'must be a lowercase 64-character')
      assert_equal(1, calls.length)

      revive_arguments = [
        '--require-runtime',
        '--authority-dir', '/run/user/1000/dev-workspaces/example-workspace/authority',
        '--portal-command', '/nix/store/portal/bin/workspace-portal',
        '--', 'revive', '2026-06-06-demo', '--as-is', '--portal-authorized',
        '--portal-operation-id', 'b' * 64
      ]
      revive = DevSession::CLI.new(
        revive_arguments,
        input: StringIO.new,
        out: StringIO.new,
        err: StringIO.new,
        cgroup_file: cgroup,
        env: {}
      )
      revive.define_singleton_method(:runner) { fake_runner }
      assert_equal(0, revive.run)
      assert_equal(
        [
          '2026-06-06-demo',
          { as_is: true, allow_abandoned: false, operation_id: 'b' * 64 }
        ],
        calls.last
      )

      missing_revive_id = DevSession::CLI.new(
        revive_arguments.first(revive_arguments.length - 2),
        input: StringIO.new,
        out: StringIO.new,
        err: (missing_revive_id_error = StringIO.new),
        cgroup_file: cgroup,
        env: {}
      )
      missing_revive_id.define_singleton_method(:runner) { fake_runner }
      assert_equal(1, missing_revive_id.run)
      assert_includes(
        missing_revive_id_error.string,
        'must be a lowercase 64-character'
      )
      assert_equal(2, calls.length)

      remove_arguments = [
        '--require-runtime',
        '--authority-dir', '/run/user/1000/dev-workspaces/example-workspace/authority',
        '--portal-command', '/nix/store/portal/bin/workspace-portal',
        '--', 'delete', '2026-06-06-demo', '--as-is', '--portal-authorized', '--force',
        '--portal-operation-id', 'a' * 64
      ]
      remove = DevSession::CLI.new(
        remove_arguments,
        input: StringIO.new,
        out: StringIO.new,
        err: StringIO.new,
        cgroup_file: cgroup,
        env: {}
      )
      remove.define_singleton_method(:runner) { fake_runner }
      assert_equal(0, remove.run)
      assert_equal(
        ['2026-06-06-demo', { as_is: true, force: true, operation_id: 'a' * 64 }],
        calls.last
      )

      File.write(cgroup, "0::/user.slice/workspace-tmux@example-workspace.service\n")
      err = StringIO.new
      rejected = DevSession::CLI.new(
        arguments,
        input: StringIO.new,
        out: StringIO.new,
        err:,
        cgroup_file: cgroup,
        env: {}
      )
      rejected.define_singleton_method(:runner) { fake_runner }
      assert_equal(1, rejected.run)
      assert_includes(err.string, 'not available to this process')
      assert_equal(3, calls.length)
    end
  end

  def test_portal_operation_identity_is_not_accepted_by_interactive_commands
    %w[archive revive].each do |command|
      calls = []
      fake_runner = Object.new
      fake_runner.define_singleton_method(:resolve_slug) do |input, as_is:|
        input if as_is
      end
      fake_runner.define_singleton_method(command) do |input, **options|
        calls << [input, options]
      end
      err = StringIO.new
      cli = DevSession::CLI.new(
        [
          command, '2026-06-06-demo', '--as-is',
          '--portal-operation-id', 'a' * 64
        ],
        input: StringIO.new,
        out: StringIO.new,
        err:
      )
      cli.define_singleton_method(:runner) { fake_runner }

      assert_equal(1, cli.run)
      assert_empty(calls)
      assert_includes(err.string, 'requires --portal-authorized')
    end
  end

  def test_cli_accepts_an_exactly_maximum_size_interactive_request
    captured = nil
    fake_runner = Object.new
    fake_runner.define_singleton_method(:start_requires_initial_goal?) do |_input, **_options|
      true
    end
    fake_runner.define_singleton_method(:start) do |_input, **options|
      captured = options.fetch(:goal_text)
    end
    request = 'x' * DevSession::MAX_MESSAGE_BYTES
    cli = DevSession::CLI.new(
      ['start', 'demo', '--no-attach'],
      input: TTYInput.new("#{request}\n"),
      out: StringIO.new,
      err: StringIO.new
    )
    cli.define_singleton_method(:runner) { fake_runner }

    assert_equal(0, cli.run)
    assert_equal(DevSession::MAX_MESSAGE_BYTES, captured.bytesize)
    assert_equal(request, captured)
  end

  def test_new_shared_session_requires_an_initial_request_before_writes
    with_workspace do |workspace|
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/codex.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        portal_command: ['/bin/true'],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(DevSession::Error) do
        runner.start('demo', as_is: false, new: false, attach: false, run_codex: true)
      end
      assert_includes(error.message, 'requires an initial request')
      refute(File.exist?(File.join(workspace, 'work', '2026-06-06-demo')))
      refute(File.exist?(File.join(workspace, 'worktrees', '2026-06-06-demo')))
    end
  end

  def test_goal_file_must_not_be_a_symlink
    with_workspace do |workspace|
      target = File.join(workspace, 'goal-target.txt')
      link = File.join(workspace, 'goal-link.txt')
      File.write(target, "Do the work.\n")
      File.symlink(target, link)

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).send(:read_goal, link)
      end
      assert_includes(error.message, 'goal file is a symlink')
    end
  end

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

  def portal_creation_expectation(workspace, source_slug, destination:, id: 'a' * 64)
    directory = File.join(workspace, '.portal-private')
    FileUtils.mkdir_p(directory, mode: 0o700)
    write_portal_creation_acceptance(workspace, destination, id: id, directory: directory)
    source = File.join(workspace, 'work', source_slug)
    manifest = YAML.safe_load(File.read(File.join(source, 'portal.yml')))
    stat = File.lstat(source)
    identity = Digest::SHA256.hexdigest([
      source, stat.dev, stat.ino, stat.ctime.to_i, stat.ctime.nsec,
      manifest.dig('codex', 'thread_id')
    ].join("\0"))
    {
      creation_receipt_id: id, creation_evidence: File.join(directory, "#{id}.complete.json"),
      expected_source: source_slug, expected_source_thread: manifest.dig('codex', 'thread_id'),
      expected_source_identity: identity
    }
  end

  def write_portal_creation_acceptance(workspace, slug, id:, directory:)
    # Only the receipt identity and history are inputs to the Ruby boundary;
    # Go tests own the full accepted request serialization.
    receipt = {
      'schema' => 1, 'workspace' => workspace, 'receiptId' => id,
      'request' => { 'slug' => slug },
      'deletionHistorySha256' => runner_for(workspace).send(:completed_removal_history, slug)
    }
    path = File.join(directory, "#{slug}.json")
    File.write(path, JSON.generate(receipt))
    File.chmod(0o600, path)
    path
  end

  def test_portal_fork_persists_receipt_evidence_before_removing_its_journal
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination = '2026-06-06-proof'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: destination)
      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination, mark: '1', slug: destination,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      observed = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
        define_method(:create_portal_fork) { |*_args, **_kwargs| 'thread-fork' }
        define_method(:name_portal_thread) { |*_args| nil }
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_args, **_kwargs| session }
        define_method(:delete_fork_journal) do |slug|
          observed << JSON.parse(File.read(options.fetch(:creation_evidence)))
          raise 'journal removed before evidence' unless File.file?(fork_journal_file(slug))
          super(slug)
        end
      end
      runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                err: StringIO.new, today: TODAY, env: {})
      runner.fork(source_slug, destination, as_is: true, json: true,
                  model: 'model-1', effort: 'high', **options)
      assert_equal(1, observed.length)
      evidence = observed.fetch(0)
      assert_equal(options.fetch(:creation_receipt_id), evidence.fetch('receiptId'))
      assert_equal('thread-source', evidence.fetch('sourceThreadId'))
      assert_equal('thread-fork', evidence.fetch('threadId'))
      assert_equal('model-1', evidence.fetch('model'))
      assert_equal(File.stat(File.join(workspace, 'work', destination)).ino, evidence.fetch('trackingInode'))
      refute(File.exist?(setup.send(:fork_journal_file, destination)))
      assert_equal(0o600, File.stat(options.fetch(:creation_evidence)).mode & 0o777)
    end
  end

  def test_portal_fork_retries_an_evidence_failure_without_forking_twice
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination = '2026-06-06-proof'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: destination)
      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination, mark: '1', slug: destination,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      observed = []
      fork_calls = []
      evidence_attempts = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
        define_method(:create_portal_fork) do |*_args, **_kwargs|
          fork_calls << true
          'thread-fork'
        end
        define_method(:write_portal_creation_evidence!) do |*args|
          evidence_attempts << true
          raise DevSession::Error, 'simulated evidence persistence failure' if evidence_attempts.length == 1
          super(*args)
        end
        define_method(:name_portal_thread) { |*_args| nil }
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_args, **_kwargs| session }
        define_method(:delete_fork_journal) do |slug|
          observed << JSON.parse(File.read(options.fetch(:creation_evidence)))
          raise 'journal removed before evidence' unless File.file?(fork_journal_file(slug))
          super(slug)
        end
      end
      runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                err: StringIO.new, today: TODAY, env: {})
      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **options)
      end
      assert_includes(error.message, 'evidence persistence failure')
      assert(File.file?(setup.send(:fork_journal_file, destination)))
      refute(File.exist?(options.fetch(:creation_evidence)))
      wrong = options.merge(creation_receipt_id: 'b' * 64)
      assert_raises(DevSession::Error) do
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **wrong)
      end
      runner.fork(source_slug, destination, as_is: true, json: true,
                  model: 'model-1', effort: 'high', **options)
      assert_equal(1, fork_calls.length)
      assert_equal(1, observed.length)
      evidence = observed.fetch(0)
      assert_equal(options.fetch(:creation_receipt_id), evidence.fetch('receiptId'))
      assert_equal('thread-source', evidence.fetch('sourceThreadId'))
      assert_equal('thread-fork', evidence.fetch('threadId'))
      assert_equal('model-1', evidence.fetch('model'))
      assert_equal(File.stat(File.join(workspace, 'work', destination)).ino, evidence.fetch('trackingInode'))
      refute(File.exist?(setup.send(:fork_journal_file, destination)))
      assert_equal(0o600, File.stat(options.fetch(:creation_evidence)).mode & 0o777)
    end
  end

  def test_portal_fork_recovers_its_bound_journal_after_source_archive_or_deletion
    %w[archive delete].each do |source_action|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        destination = '2026-06-06-recovery'
        setup = runner_for(workspace)
        setup.ensure_tracking_files(source_slug)
        manifest = setup.send(:ensure_portal_manifest, source_slug)
        manifest['codex'] = { 'thread_id' => 'thread-source' }
        setup.send(:write_portal_manifest, source_slug, manifest)
        options = portal_creation_expectation(workspace, source_slug, destination: destination)
        session = DevSession::Tmux::Session.new(
          id: '$fork', name: destination, mark: '1', slug: destination,
          workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
        )
        forks = []
        tmux_attempts = []
        runner_class = Class.new(DevSession::Runner) do
          define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
          define_method(:create_portal_fork) do |*_args, **_kwargs|
            forks << true
            'thread-fork'
          end
          define_method(:name_portal_thread) { |*_args| nil }
          define_method(:create_tmux_session) do |*_args, **kwargs|
            tmux_attempts << true
            raise DevSession::Error, 'interrupted after journal and thread publication' if tmux_attempts.length == 1
            session.identity_token = kwargs.fetch(:identity_token)
            session
          end
          define_method(:sync_slug) { |*_args, **_kwargs| session }
        end
        runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                  err: StringIO.new, today: TODAY, env: {})
        assert_raises(DevSession::Error) do
          runner.fork(source_slug, destination, as_is: true, json: true,
                      model: 'model-1', effort: 'high', **options)
        end
        journal_path = setup.send(:fork_journal_file, destination)
        assert(File.file?(journal_path))
        assert(File.file?(options.fetch(:creation_evidence) + '.request'))
        source_path = File.join(workspace, 'work', source_slug)
        if source_action == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(source_path, File.join(workspace, 'archive', source_slug))
        else
          FileUtils.rm_r(source_path)
        end
        assert_raises(DevSession::Error) do
          runner.fork(source_slug, destination, as_is: true, json: true,
                      model: 'model-1', effort: 'high', **options.merge(expected_source_thread: 'other-thread'))
        end
        assert_raises(DevSession::Error) do
          runner.fork(source_slug, destination, as_is: true, json: true,
                      model: 'model-1', effort: 'high', **options.merge(creation_receipt_id: 'b' * 64))
        end
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **options)
        proof = JSON.parse(File.read(options.fetch(:creation_evidence)))
        assert_equal(options.fetch(:creation_receipt_id), proof.fetch('receiptId'))
        assert_equal('thread-source', proof.fetch('sourceThreadId'))
        assert_equal('thread-fork', proof.fetch('threadId'))
        assert_equal(1, forks.length)
        refute(File.exist?(journal_path))
      end
    end
  end

  def test_portal_fork_refuses_a_replaced_source_before_creating_destination_state
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: '2026-06-06-replaced')
      manifest['codex']['thread_id'] = 'replacement-thread'
      setup.send(:write_portal_manifest, source_slug, manifest)
      error = assert_raises(DevSession::Error) do
        setup.fork(source_slug, '2026-06-06-replaced', as_is: true, json: true, **options)
      end
      assert_includes(error.message, 'source session changed')
      refute(File.exist?(File.join(workspace, 'work', '2026-06-06-replaced')))
      refute(File.exist?(options.fetch(:creation_evidence) + '.request'))
    end
  end

  def test_portal_creation_binding_preserves_request_and_refuses_an_unrelated_journal
    with_workspace do |workspace|
      runner = runner_for(workspace)
      directory = File.join(workspace, '.portal-private')
      FileUtils.mkdir_p(directory, mode: 0o700)
      receipt = runner.send(:creation_receipt_options, 'a' * 64, File.join(directory, 'complete.json'),
                            expected_source: nil, expected_source_thread: nil, expected_source_identity: nil)
      write_portal_creation_acceptance(workspace, '2026-06-06-new', id: 'a' * 64, directory: directory)
      runner.send(:prepare_portal_creation_receipt!, receipt, '2026-06-06-new', 'start',
                  goal: 'Accepted request', model: 'model-1', effort: 'high')
      error = assert_raises(DevSession::Error) do
        runner.send(:prepare_portal_creation_receipt!, receipt, '2026-06-06-new', 'start',
                    goal: 'Changed request', model: 'model-1', effort: 'high')
      end
      assert_includes(error.message, 'does not match')
      second = runner.send(:creation_receipt_options, 'b' * 64, File.join(directory, 'other.json'),
                           expected_source: nil, expected_source_thread: nil, expected_source_identity: nil)
      write_portal_creation_acceptance(workspace, '2026-06-06-unrelated', id: 'b' * 64, directory: directory)
      runner.send(:prepare_start_journal!, '2026-06-06-unrelated')
      assert_raises(DevSession::Error) do
        runner.send(:prepare_portal_creation_receipt!, second, '2026-06-06-unrelated', 'start',
                    goal: 'Accepted request', model: 'model-1', effort: 'high')
      end
      refute(File.exist?(second.fetch('evidence') + '.request'))
    end
  end

  def test_portal_creation_requires_its_exact_current_receipt_even_with_a_binding
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-current-receipt'
      directory = File.join(workspace, '.portal-private')
      FileUtils.mkdir_p(directory, mode: 0o700)
      path = write_portal_creation_acceptance(workspace, slug, id: 'a' * 64, directory: directory)
      original = File.read(path)
      receipt = runner.send(:creation_receipt_options, 'a' * 64, File.join(directory, 'complete.json'),
                            expected_source: nil, expected_source_thread: nil, expected_source_identity: nil)
      prepare = lambda do
        runner.send(:prepare_portal_creation_receipt!, receipt, slug, 'start',
                    goal: 'Accepted request', model: 'model-1', effort: 'high')
      end
      prepare.call
      binding = File.read(receipt.fetch('evidence') + '.request')
      %w[missing replaced workspace slug schema missing-history invalid-history].each do |change|
        current = JSON.parse(original)
        case change
        when 'replaced' then current['receiptId'] = 'b' * 64
        when 'workspace' then current['workspace'] += '-different'
        when 'slug' then current['request']['slug'] = 'different-slug'
        when 'schema' then current['schema'] = 99
        when 'missing-history' then current.delete('deletionHistorySha256')
        when 'invalid-history' then current['deletionHistorySha256'] = 'not-a-digest'
        end
        File.write(path, JSON.generate(current))
        File.chmod(0o600, path)
        File.unlink(path) if change == 'missing'
        error = assert_raises(DevSession::Error, &prepare)
        assert_includes(error.message, 'no longer current')
        assert_equal(binding, File.read(receipt.fetch('evidence') + '.request'))
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
      end
      File.write(path, original)
      File.chmod(0o600, path)
      prepare.call
      changed = JSON.parse(original).merge('deletionHistorySha256' => 'c' * 64)
      File.write(path, JSON.generate(changed))
      error = assert_raises(DevSession::Error, &prepare)
      assert_includes(error.message, 'does not match the recorded CLI request')
      assert_equal(binding, File.read(receipt.fetch('evidence') + '.request'))
    end
  end

  def test_portal_fork_rechecks_current_receipt_after_source_validation
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination = '2026-06-06-queued-fork'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: destination)
      current_path = File.join(File.dirname(options.fetch(:creation_evidence)), "#{destination}.json")
      runner_class = Class.new(DevSession::Runner) do
        define_method(:resolve_portal_fork_settings) do |*_args, **_kwargs|
          File.unlink(current_path)
          ['model-1', 'high']
        end
        define_method(:create_portal_fork) { |*_args, **_kwargs| raise 'retired fork reached thread creation' }
      end
      runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                err: StringIO.new, today: TODAY, env: {})
      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **options)
      end
      assert_includes(error.message, 'no longer current')
      refute(File.exist?(runner.send(:fork_journal_file, destination)))
      refute(File.exist?(File.join(workspace, 'work', destination)))
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
        case ARGV[0, 2]
        when ['thread', 'resolve-fork-settings']
          puts JSON.generate(model: 'gpt-test', reasoningEffort: 'xhigh')
        when ['thread', 'fork']
          puts JSON.generate(threadId: 'thread-fork')
        end
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
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
      commands = File.readlines(log, chomp: true)
      preflight = commands.find { |line| line.start_with?('thread resolve-fork-settings ') }
      assert_includes(preflight, '--thread-id thread-source')
      command = commands.find { |line| line.start_with?('thread fork ') }
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

      error = assert_raises(DevSession::Error) do
        runner.fork('2026-06-05-source', 'taken', as_is: false, json: true)
      end
      assert_includes(error.message, 'already exists')
    end
  end

  def test_fork_rejects_a_source_conversation_from_another_runtime_without_mutation
    with_workspace do |workspace|
      source_slug = '2026-06-05-old-runtime'
      destination_slug = '2026-06-06-fork'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = {
        'thread_id' => 'thread-old',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      setup.send(:write_portal_manifest, source_slug, manifest)
      called = File.join(workspace, 'portal-called')
      portal = [RbConfig.ruby, '-e', "File.write(#{called.dump}, 'called'); exit 1"]
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: portal,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, 'fork', as_is: false, json: true)
      end

      assert_includes(error.message, 'belongs to another runtime')
      refute(File.exist?(called))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
      refute(File.exist?(File.join(workspace, 'worktrees', destination_slug)))
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
        if ARGV[0, 2] == ['thread', 'resolve-fork-settings']
          puts JSON.generate(model: 'source-model', reasoningEffort: 'medium')
        elsif ARGV[0, 2] == ['thread', 'fork']
          File.open(#{calls.dump}, 'a') { |file| file.puts 'fork' }
          puts JSON.generate(threadId: 'thread-fork')
        end
      RUBY
      failing_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_args, **_kwargs|
          raise DevSession::Error, 'tmux failed'
        end
      end
      failing = failing_class.new(
        workspace:, tmux: NullTmux.new, portal_command: [RbConfig.ruby, portal],
        out: StringIO.new, err: StringIO.new, today: TODAY, env: {}
      )
      assert_raises(DevSession::Error) do
        failing.fork(source_slug, 'retry', as_is: false, json: true)
      end

      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
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

  def test_fork_resumes_after_manifest_crash_on_the_next_day_without_source_tracking
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      crashing_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |*_arguments, **_keywords|
          raise DevSession::Error, 'simulated crash before forked thread creation'
        end
      end
      crashing = crashing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        crashing.fork(
          source_slug,
          'retry',
          as_is: false,
          json: true,
          model: 'gpt-test',
          effort: 'xhigh'
        )
      end
      journal_path = setup.send(:fork_journal_file, destination_slug)
      assert(File.file?(journal_path))
      journal = JSON.parse(File.read(journal_path))
      assert_equal('thread-source', journal.fetch('source_thread_id'))
      assert_equal('gpt-test', journal.fetch('model'))
      assert_equal('xhigh', journal.fetch('effort'))
      partial = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, partial.fetch('forked_from'))
      assert_nil(partial.dig('codex', 'thread_id'))
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))

      session = DevSession::Tmux::Session.new(
        id: '$12', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      forked_from_thread = nil
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |_slug, source_thread_id, **_keywords|
          forked_from_thread = source_thread_id
          'thread-fork'
        end
        define_method(:name_portal_thread) { |*_arguments| nil }
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          session.identity_token = keywords.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
      end
      out = StringIO.new
      retry_runner = retry_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        retry_runner.fork(
          'source',
          'retry',
          as_is: false,
          json: true,
          model: 'gpt-other',
          effort: 'xhigh'
        )
      end
      assert_includes(error.message, 'options do not match')
      assert(File.file?(journal_path))
      retry_runner.fork(
        'source',
        'retry',
        as_is: false,
        json: true,
        model: 'gpt-test',
        effort: 'xhigh'
      )

      result = JSON.parse(out.string)
      assert_equal(destination_slug, result.fetch('slug'))
      assert_equal(source_slug, result.fetch('forkedFrom'))
      assert_equal('thread-source', forked_from_thread)
      refute(File.exist?(journal_path))
      refute(File.exist?(File.join(workspace, 'work', '2026-06-07-retry')))
    end
  end

  def test_fork_recovers_a_journal_owned_partial_tracking_skeleton
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      crashing_class = Class.new(DevSession::Runner) do
        def ensure_fork_tracking_file!(path, expected, label)
          super
          if label == 'plan.md'
            raise DevSession::Error, 'simulated crash between tracking files'
          end
        end
      end
      crashing = crashing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        crashing.fork(source_slug, 'retry', as_is: false, json: true)
      end
      assert(File.file?(setup.send(:fork_journal_file, destination_slug)))
      destination_work = File.join(workspace, 'work', destination_slug)
      plan_path = File.join(destination_work, 'plan.md')
      state_path = File.join(destination_work, 'state.md')
      assert(File.file?(plan_path))
      refute(File.exist?(state_path))
      File.link(plan_path, File.join(destination_work, '.plan.md.123.tmp'))
      previous_umask = File.umask(0o077)
      begin
        File.write(File.join(destination_work, '.state.md.123.tmp'), "partial\n")
      ensure
        File.umask(previous_umask)
      end
      assert_equal(
        0o600,
        File.stat(File.join(destination_work, '.state.md.123.tmp')).mode & 0o777
      )
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |*_arguments, **_keywords|
          raise DevSession::Error, 'reached thread creation'
        end
      end
      retry_runner = retry_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        retry_runner.fork('source', 'retry', as_is: false, json: true)
      end

      assert_equal('reached thread creation', error.message)
      state = File.read(state_path)
      assert_equal(setup.send(:state_skeleton, destination_slug), state)
      refute(File.exist?(File.join(destination_work, '.plan.md.123.tmp')))
      refute(File.exist?(File.join(destination_work, '.state.md.123.tmp')))
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, manifest.fetch('forked_from'))
    end
  end

  def test_fork_recovers_a_journal_owned_default_manifest
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      crashing_class = Class.new(DevSession::Runner) do
        def ensure_fork_portal_manifest(slug, _source_slug)
          write_portal_manifest(slug, new_portal_manifest(slug))
          raise DevSession::Error, 'simulated crash before fork provenance'
        end
      end
      crashing = crashing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        crashing.fork(source_slug, 'retry', as_is: false, json: true)
      end
      partial = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      refute(partial.key?('forked_from'))
      portal_temporary = File.join(
        workspace, 'work', destination_slug, '.portal.yml.456.tmp'
      )
      File.write(portal_temporary, "partial: true\n")
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |*_arguments, **_keywords|
          raise DevSession::Error, 'reached thread creation'
        end
      end
      retry_runner = retry_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        retry_runner.fork('source', 'retry', as_is: false, json: true)
      end

      assert_equal('reached thread creation', error.message)
      recovered = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, recovered.fetch('forked_from'))
      refute(File.exist?(portal_temporary))
    end
  end

  def test_fork_revalidates_options_when_a_journal_appears_under_lock
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      load_count = 0
      racing_class = Class.new(DevSession::Runner) do
        define_method(:load_fork_journal) do |slug, expected_source = nil|
          load_count += 1
          if load_count == 2
            prepare_fork_journal!(
              destination_slug,
              source_slug,
              'thread-source',
              model: 'gpt-first',
              effort: 'xhigh'
            )
          end
          super(slug, expected_source)
        end
      end
      runner = racing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        runner.fork(
          source_slug,
          'alternative',
          as_is: false,
          json: true,
          model: 'gpt-second',
          effort: 'xhigh'
        )
      end

      assert_includes(error.message, 'options do not match')
      journal = JSON.parse(File.read(setup.send(:fork_journal_file, destination_slug)))
      assert_equal('gpt-first', journal.fetch('model'))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
    end
  end

  def test_fork_rejects_archive_history_before_publishing_its_journal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      archive = File.join(workspace, 'archive', destination_slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Archived\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n\n# #{destination_slug}\n"
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('archive', destination_slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'record archived slug')
      FileUtils.rm_r(archive)
      assert_git_success(
        'git', '-C', workspace, 'add', '-A', '--', File.join('archive', destination_slug)
      )
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'remove archive checkout')

      error = assert_raises(DevSession::Error) do
        setup.fork(source_slug, 'alternative', as_is: false, json: true)
      end

      assert_includes(error.message, 'archived slug cannot be reused')
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
    end
  end

  def test_fork_rejects_invalid_settings_before_publishing_its_journal
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        warn 'Codex model is unavailable'
        exit 1
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::CommandError) do
        runner.fork(
          source_slug,
          'alternative',
          as_is: false,
          json: true,
          model: 'missing-model',
          effort: 'xhigh'
        )
      end

      assert_includes(error.message, 'Codex model is unavailable')
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
    end
  end

  def test_fork_recovery_rejects_a_renamed_authority_session
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      authority_dir = File.join(workspace, 'authority')
      setup = runner_for(workspace, authority_dir:)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', destination_slug))
      original = DevSession::Tmux::Session.new(
        id: '$11', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, environment_slug: destination_slug,
        socket_path: '/run/test.sock', identity_token: 'a' * 64
      )
      setup.send(:write_session_authority, destination_slug, original, state: 'ready')
      tmux = RenamedManagedTmux.new(
        destination_slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)

      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, 'alternative', as_is: false, json: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.file?(File.join(authority_dir, "#{destination_slug}.json")))
    end
  end

  def test_fork_recovery_replaces_only_the_journal_bound_partial_session
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      authority_dir = File.join(workspace, 'authority')
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = PartialManagedTmux.new(
        destination_slug, workspace:, identity_token: journal.fetch('tmux_identity')
      )
      created = DevSession::Tmux::Session.new(
        id: '$12', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, environment_slug: destination_slug,
        socket_path: '/run/test.sock', codex_thread_id: 'thread-fork',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          created.identity_token = keywords.fetch(:identity_token)
          created
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| created }
      end
      runner = runner_class.new(
        workspace:, tmux:, authority_dir:, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.fork('source', 'alternative', as_is: false, json: true)

      assert(tmux.killed)
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      authority = JSON.parse(
        File.read(File.join(authority_dir, "#{destination_slug}.json"))
      )
      assert_equal(journal.fetch('tmux_identity'), authority.fetch('tmux_identity'))
    end
  end

  def test_fork_recovery_refuses_a_partial_session_with_another_identity
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = PartialManagedTmux.new(
        destination_slug, workspace:, identity_token: 'b' * 64
      )
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, 'alternative', as_is: false, json: true)
      end

      assert_includes(error.message, 'does not match fork identity')
      refute(tmux.killed)
      assert(File.exist?(setup.send(:fork_journal_file, destination_slug)))
    end
  end

  def test_completed_fork_recovery_does_not_require_source_tracking
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = ManagedTmux.new(
        destination_slug,
        workspace:,
        identity_token: journal.fetch('tmux_identity'),
        codex_thread_id: 'thread-fork'
      )
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))
      out = StringIO.new
      runner = runner_for(workspace, tmux:, out:)

      runner.fork('source', 'alternative', as_is: false, json: true)

      refute(tmux.killed)
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      assert_equal('thread-fork', JSON.parse(out.string).fetch('threadId'))
    end
  end

  def test_completed_fork_recovery_uses_the_journal_source_when_suffix_is_reused
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      newer_source_slug = '2026-06-07-source'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      setup.ensure_tracking_files(newer_source_slug)
      tmux = ManagedTmux.new(
        destination_slug,
        workspace:,
        identity_token: journal.fetch('tmux_identity'),
        codex_thread_id: 'thread-fork'
      )
      out = StringIO.new
      runner = runner_for(workspace, tmux:, out:)

      runner.fork('source', 'alternative', as_is: false, json: true)

      result = JSON.parse(out.string)
      assert_equal(source_slug, result.fetch('forkedFrom'))
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
    end
  end

  def test_completed_fork_recovery_reuses_the_journal_destination_after_midnight
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = ManagedTmux.new(
        destination_slug,
        workspace:,
        identity_token: journal.fetch('tmux_identity'),
        codex_thread_id: 'thread-fork'
      )
      out = StringIO.new
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        out:,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.fork(source_slug, 'alternative', as_is: false, json: true)

      result = JSON.parse(out.string)
      assert_equal(destination_slug, result.fetch('slug'))
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      refute(File.exist?(File.join(workspace, 'work', '2026-06-07-alternative')))
    end
  end

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
        File.read(File.expand_path('../portal/internal/session/runtime-contract.json', __dir__))
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
        File.read(File.expand_path('../portal/internal/session/runtime-contract.json', __dir__))
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
      session = DevSession::Tmux::Session.new(
        id: '$created',
        name: slug,
        mark: '1',
        slug:,
        workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |
          _slug, run_codex:, thread_id:, launch_codex:, identity_token:
        |
          raise 'missing shared thread' unless run_codex && thread_id == 'thread-123'
          raise 'Codex launched before the initial request persisted' if launch_codex
          raise 'missing journaled tmux identity' unless identity_token&.match?(/\A[0-9a-f]{64}\z/)

          session
        end

        define_method(:sync_slug) do |_slug, require_session:, session:|
          raise 'missing created session' unless require_session && session

          session
        end

        define_method(:revalidate_session!) do |expected|
          expected
        end

        define_method(:reconcile_native_client!) do |_slug, expected, **_keywords|
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
      assert_includes(File.read(File.join(workspace, 'work', slug, 'state.md')), 'initial request')
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

  def test_portal_start_persists_receipt_bound_completion_for_the_exact_goal
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
      session = DevSession::Tmux::Session.new(
        id: '$created',
        name: slug,
        mark: '1',
        slug:,
        workspace:, codex_thread_id: 'thread-123'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |
          _slug, run_codex:, thread_id:, launch_codex:, identity_token:
        |
          raise 'missing shared thread' unless run_codex && thread_id == 'thread-123'
          raise 'Codex launched before the initial request persisted' if launch_codex
          raise 'missing journaled tmux identity' unless identity_token&.match?(/\A[0-9a-f]{64}\z/)

          session
        end

        define_method(:sync_slug) do |_slug, require_session:, session:|
          raise 'missing created session' unless require_session && session

          session
        end

        define_method(:revalidate_session!) do |expected|
          expected
        end

        define_method(:reconcile_native_client!) do |_slug, expected, **_keywords|
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

      private_directory = File.join(workspace, '.portal-private')
      FileUtils.mkdir_p(private_directory, mode: 0o700)
      evidence_path = File.join(private_directory, 'complete.json')
      write_portal_creation_acceptance(workspace, slug, id: 'a' * 64, directory: private_directory)
      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true, creation_receipt_id: "a" * 64, creation_evidence: evidence_path
      )

      proof = JSON.parse(File.read(evidence_path))
      assert_equal('a' * 64, proof.fetch('receiptId'))
      assert_equal('thread-123', proof.fetch('threadId'))
      assert_equal(Digest::SHA256.hexdigest('Implement a useful feature.'), proof.fetch('goalSha256'))
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal(%w[goal_sha256 run_codex schema slug state], journal.keys.sort)
      result = JSON.parse(out.string)
      assert_equal(slug, result['slug'])
      assert_equal('thread-123', result['threadId'])
      assert_equal("https://workspace.example.test/#{slug}/", result['url'])
      assert_includes(File.read(File.join(workspace, 'work', slug, 'plan.md')), 'Implement a useful feature.')
      assert_includes(File.read(File.join(workspace, 'work', slug, 'state.md')), 'initial request')
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

  def test_portal_plan_start_recovers_evidence_failure_after_source_archive_or_deletion
    %w[archive delete].each do |source_action|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        slug = '2026-06-06-plan-recovery'
        setup = runner_for(workspace)
        setup.ensure_tracking_files(source_slug)
        source = setup.send(:ensure_portal_manifest, source_slug)
        source['codex'] = { 'thread_id' => 'source-thread' }
        setup.send(:write_portal_manifest, source_slug, source)
        options = portal_creation_expectation(workspace, source_slug, destination: slug)
        goal = "Implement the following approved plan from session #{source_slug}.\n\nExact accepted plan."
        created = []
        delivered = []
        evidence_attempts = []
        session = DevSession::Tmux::Session.new(
          id: '$plan', name: slug, mark: '1', slug:, workspace:, codex_thread_id: 'plan-thread'
        )
        live = false
        runner_class = Class.new(DevSession::Runner) do
          define_method(:create_portal_thread) do |*_args, **_kwargs|
            created << 'plan-thread'
            'plan-thread'
          end
          define_method(:name_portal_thread) { |*_args| nil }
          define_method(:create_tmux_session) do |*_args, **kwargs|
            session.identity_token = kwargs.fetch(:identity_token)
            live = true
            session
          end
          define_method(:cleanup_session) { |_slug| live ? session : nil }
          define_method(:sync_slug) { |*_args, **_kwargs| session }
          define_method(:revalidate_session!) { |expected| expected }
          define_method(:reconcile_native_client!) { |_slug, expected, **_kwargs| expected }
          define_method(:send_portal_goal) do |_slug, _thread_id, goal_file, **_kwargs|
            delivered << read_goal(goal_file)
          end
          define_method(:write_portal_creation_evidence!) do |*args|
            evidence_attempts << true
            raise DevSession::Error, 'interrupted before completion evidence' if evidence_attempts.length == 1
            super(*args)
          end
        end
        runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                  err: StringIO.new, today: TODAY, env: {})
        arguments = { as_is: true, new: false, attach: false, run_codex: true, json: true,
                      exclusive: true, model: 'model-1', effort: 'high', goal_text: goal, **options }
        assert_raises(DevSession::Error) { runner.start(slug, **arguments) }
        journal_path = runner.send(:creation_journal_file, slug)
        original_journal = File.read(journal_path)
        assert_equal('ready', JSON.parse(original_journal).fetch('state'))
        refute(File.exist?(options.fetch(:creation_evidence)))
        source_path = File.join(workspace, 'work', source_slug)
        if source_action == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(source_path, File.join(workspace, 'archive', source_slug))
        else
          FileUtils.rm_r(source_path)
        end
        assert_raises(DevSession::Error) { runner.start(slug, **arguments.merge(goal_text: 'A different plan')) }
        foreign_journal = JSON.parse(original_journal).merge('goal_sha256' => Digest::SHA256.hexdigest('A different plan'))
        File.write(journal_path, JSON.generate(foreign_journal))
        assert_raises(DevSession::Error) { runner.start(slug, **arguments) }
        File.write(journal_path, original_journal)
        runner.start(slug, **arguments)
        proof = JSON.parse(File.read(options.fetch(:creation_evidence)))
        assert_equal(options.fetch(:creation_receipt_id), proof.fetch('receiptId'))
        assert_equal('source-thread', proof.fetch('sourceThreadId'))
        assert_equal('plan-thread', proof.fetch('threadId'))
        assert_equal(Digest::SHA256.hexdigest(goal), proof.fetch('goalSha256'))
        assert_equal('model-1', proof.fetch('model'))
        assert_equal('high', proof.fetch('effort'))
        assert_equal(['plan-thread'], created)
        assert_equal([goal], delivered)
        assert_equal(original_journal, File.read(journal_path))
      end
    end
  end

  def test_portal_plan_start_binding_alone_still_requires_its_source
    %w[archive delete replace].each do |source_action|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        slug = '2026-06-06-plan-fresh'
        runner = runner_for(workspace)
        runner.ensure_tracking_files(source_slug)
        source = runner.send(:ensure_portal_manifest, source_slug)
        source['codex'] = { 'thread_id' => 'source-thread' }
        runner.send(:write_portal_manifest, source_slug, source)
        options = portal_creation_expectation(workspace, source_slug, destination: slug)
        receipt = runner.send(:creation_receipt_options, options.fetch(:creation_receipt_id), options.fetch(:creation_evidence),
                              expected_source: options.fetch(:expected_source), expected_source_thread: options.fetch(:expected_source_thread),
                              expected_source_identity: options.fetch(:expected_source_identity))
        runner.send(:prepare_portal_creation_receipt!, receipt, slug, 'start', goal: 'Exact plan', model: 'model-1', effort: 'high')
        binding = File.read(options.fetch(:creation_evidence) + '.request')
        source_path = File.join(workspace, 'work', source_slug)
        if source_action == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(source_path, File.join(workspace, 'archive', source_slug))
        elsif source_action == 'delete'
          FileUtils.rm_r(source_path)
        else
          source['codex']['thread_id'] = 'replacement-thread'
          runner.send(:write_portal_manifest, source_slug, source)
        end
        assert_raises(DevSession::Error) do
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: true, json: true,
                       exclusive: true, model: 'model-1', effort: 'high', goal_text: 'Exact plan', **options)
        end
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
        refute(File.exist?(File.join(workspace, 'work', slug)))
        assert_equal(binding, File.read(options.fetch(:creation_evidence) + '.request'))
      end
    end
  end

  def test_portal_creation_retry_cannot_recreate_a_deleted_destination
    %w[new plan fork].each do |kind|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        slug = "2026-06-06-deleted-#{kind}"
        setup = runner_for(workspace)
        setup.ensure_tracking_files(source_slug)
        source = setup.send(:ensure_portal_manifest, source_slug)
        source['codex'] = { 'thread_id' => 'source-thread' }
        setup.send(:write_portal_manifest, source_slug, source)
        options = portal_creation_expectation(workspace, source_slug, destination: slug)
        options = options.reject { |key, _| key.to_s.start_with?('expected_source') } if kind == 'new'
        created = []
        delivered = []
        session = DevSession::Tmux::Session.new(
          id: '$deleted', name: slug, mark: '1', slug:, workspace:, codex_thread_id: 'deleted-thread'
        )
        runner_class = Class.new(DevSession::Runner) do
          define_method(:create_portal_thread) { |*_args, **_kwargs| created << kind; 'deleted-thread' }
          define_method(:create_portal_fork) { |*_args, **_kwargs| created << kind; 'deleted-thread' }
          define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
          define_method(:name_portal_thread) { |*_args| nil }
          define_method(:create_tmux_session) do |*_args, **kwargs|
            session.identity_token = kwargs.fetch(:identity_token)
            session
          end
          define_method(:sync_slug) { |*_args, **_kwargs| session }
          define_method(:revalidate_session!) { |expected| expected }
          define_method(:reconcile_native_client!) { |_slug, expected, **_kwargs| expected }
          define_method(:send_portal_goal) { |_slug, _thread, path, **_kwargs| delivered << read_goal(path) }
          define_method(:write_portal_creation_evidence!) { |*_args| raise DevSession::Error, 'interrupted before receipt evidence' }
        end
        runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                  err: StringIO.new, today: TODAY,
                                  env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state'), 'PATH' => '' })
        arguments = { as_is: true, json: true, model: 'model-1', effort: 'high', **options }
        attempt = lambda do
          if kind == 'fork'
            runner.fork(source_slug, slug, **arguments)
          else
            runner.start(slug, new: false, attach: false, run_codex: true,
                         exclusive: true, goal_text: 'Exact accepted goal', **arguments)
          end
        end
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'interrupted before receipt evidence')
        assert_equal('ready', YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml'))).dig('creation', 'state'))
        refute(File.exist?(options.fetch(:creation_evidence)))
        binding_path = options.fetch(:creation_evidence) + '.request'
        original_binding = File.read(binding_path)
        original_time = File.mtime(binding_path)
        # The old generation can finish its fork journal without new receipt
        # evidence, then perform its normal deletion lifecycle.
        runner.send(:delete_fork_journal, slug) if kind == 'fork'
        removal = runner.send(:prepare_removal!, slug, force: false)
        %w[validated thread_retiring thread_retired clusters_released worktrees_removed runtime_retired].each do |phase|
          runner.send(:advance_removal!, slug, removal, phase)
        end
        worktrees = File.join(workspace, 'worktrees', slug)
        Dir.rmdir(worktrees) if File.directory?(worktrees)
        runner.send(:preserve_removed_state!, slug, removal, nil)
        runner.send(:advance_removal!, slug, removal, 'tracking_preserved')
        runner.send(:advance_removal!, slug, removal, 'tracking_committed')
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'unfinished')
        assert_equal(original_binding, File.read(binding_path))
        runner.send(:finalize_removal!, slug, removal)
        removal['removed_at'] = '2000-01-01T00:00:00Z'
        runner.send(:write_removal_metadata!, removal.fetch('recovery'), removal)
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'session was deleted')
        assert_equal(original_time, File.mtime(binding_path))
        assert_equal([kind], created)
        assert_equal(kind == 'fork' ? [] : ['Exact accepted goal'], delivered)
        refute(File.exist?(File.join(workspace, 'work', slug)))
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
        # Retirement removes the cache. A queued worker cannot reconstruct
        # acceptance from its binding, the clock, or current deletion history.
        current_path = File.join(File.dirname(binding_path), "#{slug}.json")
        File.unlink(current_path)
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'no longer current')
        File.unlink(binding_path)
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'no longer current')
        refute(File.exist?(binding_path))
        assert_equal([kind], created)
        removal['removed_at'] = '2099-01-01T00:00:00Z'
        runner.send(:write_removal_metadata!, removal.fetch('recovery'), removal)
        fresh_options = portal_creation_expectation(workspace, source_slug, destination: slug, id: 'b' * 64)
        fresh_options = fresh_options.reject { |key, _| key.to_s.start_with?('expected_source') } if kind == 'new'
        fresh = runner.send(:creation_receipt_options, fresh_options.fetch(:creation_receipt_id), fresh_options.fetch(:creation_evidence),
                            expected_source: fresh_options[:expected_source], expected_source_thread: fresh_options[:expected_source_thread],
                            expected_source_identity: fresh_options[:expected_source_identity])
        runner.send(:prepare_portal_creation_receipt!, fresh, slug, kind == 'fork' ? 'fork' : 'start',
                    goal: kind == 'fork' ? nil : 'Fresh goal', model: 'model-1', effort: 'high')
        fresh_binding = fresh_options.fetch(:creation_evidence) + '.request'
        assert(File.file?(fresh_binding))
        original_history = fresh.fetch('binding').fetch('deletion_history_sha256')
        %w[2000-01-01T00:00:00Z 2099-01-01T00:00:00Z].each do |clock|
          removal['removed_at'] = clock
          runner.send(:write_removal_metadata!, removal.fetch('recovery'), removal)
          runner.send(:prepare_portal_creation_receipt!, fresh, slug, kind == 'fork' ? 'fork' : 'start',
                      goal: kind == 'fork' ? nil : 'Fresh goal', model: 'model-1', effort: 'high')
          assert_equal(original_history, fresh.fetch('binding').fetch('deletion_history_sha256'))
        end
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'no longer current')

      end
    end
  end

  def test_start_uses_one_private_snapshot_when_the_caller_goal_file_changes
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      original_request = 'Implement the original request.'
      goal = File.join(workspace, 'goal.txt')
      delivered = File.join(workspace, 'delivered.txt')
      snapshot_record = File.join(workspace, 'snapshot-record.txt')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "#{original_request}\n")
      File.write(portal, <<~RUBY)
        require 'json'
        case ARGV[1]
        when 'create'
          puts JSON.generate(threadId: 'thread-snapshot')
        when 'ensure-initial'
          input = ARGV.fetch(ARGV.index('--input-file') + 1)
          File.binwrite(#{delivered.dump}, File.binread(input))
          File.write(#{snapshot_record.dump}, "\#{input}\n\#{File.stat(input).mode & 0o777}\n")
        end
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:prepare_creation_journal) do |*arguments, **keywords|
          journal = super(*arguments, **keywords)
          File.write(goal, "A replacement request that must be ignored.\n")
          journal
        end
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }
        define_method(:reconcile_native_client!) { |_slug, expected, **_keywords| expected }
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
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

      assert_equal(original_request, File.binread(delivered))
      snapshot_path, snapshot_mode = File.readlines(snapshot_record, chomp: true)
      assert_equal(0o600, Integer(snapshot_mode))
      refute(File.exist?(snapshot_path))
      assert_includes(
        File.read(File.join(workspace, 'work', slug, 'plan.md')),
        original_request
      )
      refute_includes(
        File.read(File.join(workspace, 'work', slug, 'plan.md')),
        'replacement request'
      )
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal(Digest::SHA256.hexdigest(original_request), journal.fetch('goal_sha256'))
    end
  end

  def test_stopped_ready_session_rejects_a_recorded_thread_without_persisted_history
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
        warn 'recorded thread is unavailable'
        exit 1
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-ready',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::CommandError) do
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
      assert_includes(command, 'thread require-materialized')
      assert_includes(command, '--thread-id thread-ready')
      assert_includes(command, "--cwd #{File.join(workspace, 'work', slug)}")
      refute_includes(command, 'thread create')
      unchanged = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-ready', unchanged.dig('codex', 'thread_id'))
      assert_equal('ready', unchanged.dig('creation', 'state'))
    end
  end

  def test_ready_creation_replay_rejects_a_thread_without_persisted_history
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "Implement a persistent conversation.\n")
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
        warn 'recorded thread has no rollout'
        exit 1
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      journal = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      runner.ensure_tracking_files(slug)
      runner.send(:seed_goal, slug, goal)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: journal)
      manifest['codex'] = {
        'thread_id' => 'thread-ready',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      manifest['creation']['state'] = 'ready'
      manifest['creation']['initial_goal_sent'] = true
      manifest['creation'].delete('initial_goal_attempted')
      manifest['schema'] = 1
      runner.send(:write_portal_manifest, slug, manifest)
      runner.send(:mark_creation_journal_ready, slug, journal)

      error = assert_raises(DevSession::CommandError) do
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

      assert_includes(error.message, 'recorded thread has no rollout')
      assert_includes(File.read(log), 'thread require-materialized')
      unchanged = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', unchanged.dig('creation', 'state'))

      error = assert_raises(DevSession::CommandError) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal,
          json: true,
          exclusive: false
        )
      end

      assert_includes(error.message, 'recorded thread has no rollout')
      assert_includes(File.read(log), 'thread require-materialized')
    end
  end

  def test_start_refuses_to_retrofit_an_unshared_running_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ManagedTmux.new(slug, workspace:)
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: nil
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      shared_runner = DevSession::Runner.new(
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
      error = assert_raises(DevSession::Error) do
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
      created_session = DevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock'
      )
      runner_class = Class.new(DevSession::Runner) do
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

      assert_raises(DevSession::CommandError) do
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

      creation_identity = JSON.parse(
        File.read(runner.send(:creation_journal_file, slug))
      ).fetch('tmux_identity')
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:, identity_token: creation_identity),
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
      replay_runner = DevSession::Runner.new(
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

      stopped_session = DevSession::Tmux::Session.new(
        id: '$restarted', name: slug, mark: '1', slug:, workspace:
      )
      restart_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| stopped_session }
        define_method(:sync_slug) { |*_arguments, **_keywords| stopped_session }
        define_method(:revalidate_session!) { |_expected| stopped_session }
      end
      restart_out = StringIO.new
      restart_runner = restart_runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: restart_out,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      restart_runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        json: true
      )
      assert_equal('thread-retry', JSON.parse(restart_out.string).fetch('threadId'))
      restarted_commands = File.readlines(log, chomp: true)
      assert_equal(3, restarted_commands.count { |line| line.start_with?('thread create ') })
      assert_equal(2, restarted_commands.count { |line| line.start_with?('thread ensure-initial ') })
    end
  end

  def test_start_discards_a_stale_authority_thread_before_recreating_tmux
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      portal_log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal')
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{portal_log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        puts JSON.generate(threadId: 'thread-manifest') if ARGV[1] == 'create'
      RUBY

      setup = DevSession::Runner.new(
        workspace:, authority_dir:, tmux: NullTmux.new,
        out: StringIO.new, err: StringIO.new, today: TODAY, env: {}
      )
      setup.ensure_tracking_files(slug)
      manifest = setup.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-manifest',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      setup.send(:write_portal_manifest, slug, manifest)
      stale = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-stale',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      setup.send(:write_session_authority, slug, stale, state: 'ready')

      created_threads = []
      created = stale.dup
      created.id = '$8'
      created.codex_thread_id = 'thread-manifest'
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |_slug, thread_id:, **_keywords|
          created_threads << thread_id
          created
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| created }
        define_method(:revalidate_session!) { |_expected| created }
      end
      out = StringIO.new
      runner = runner_class.new(
        workspace:, authority_dir:, tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock', codex_version: '0.152.1',
        codex_command: codex, tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal], out:, err: StringIO.new,
        today: TODAY, env: {}
      )

      runner.start(
        slug, as_is: true, new: false, attach: false, run_codex: true, json: true
      )

      assert_equal(['thread-manifest'], created_threads)
      assert_equal('thread-manifest', JSON.parse(out.string).fetch('threadId'))
      commands = File.readlines(portal_log, chomp: true)
      assert(commands.any? { |line| line.start_with?('thread require-materialized ') })
      create = commands.find { |line| line.start_with?('thread create ') }
      assert_includes(create, '--thread-id thread-manifest')
      {
        '--session-slug' => slug,
        '--workspace' => workspace,
        '--cwd' => File.join(workspace, 'work', slug),
        '--worktrees-dir' => File.join(workspace, 'worktrees', slug),
        '--authority-dir' => authority_dir,
        '--tmux-socket' => '/run/test/tmux.sock',
        '--codex-command' => codex,
        '--socket' => '/run/test/codex.sock',
        '--codex-version' => '0.152.1',
        '--portal-command' => RbConfig.ruby
      }.each do |argument, value|
        assert_includes(create, "#{argument} #{value}")
      end
      refute_includes(create, 'thread-stale')
      authority = JSON.parse(File.read(File.join(authority_dir, "#{slug}.json")))
      assert_equal('thread-manifest', authority.fetch('codex_thread_id'))
      assert_equal('$8', authority.fetch('tmux_session_id'))
    end
  end

  def test_start_rejects_a_renamed_authority_session
    with_workspace do |workspace|
      slug = '2026-06-06-renamed-start'
      authority_dir = File.join(workspace, 'authority')
      tmux = RenamedManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      session = DevSession::Tmux::Session.new(
        id: '$11', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, session, state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
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
      session = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-123',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }
        define_method(:reconcile_native_client!) do |_slug, expected, **_keywords|
          raise 'initial request was not persisted before Codex launch' unless File.file?(observation)

          expected
        end
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
      original = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-vanished', codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.153.0', codex_pane_id: '%1'
      )
      first_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          original.identity_token = keywords.fetch(:identity_token)
          original
        end
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
      assert_raises(DevSession::CommandError) do
        first.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end

      creation_identity = JSON.parse(
        File.read(first.send(:creation_journal_file, slug))
      ).fetch('tmux_identity')

      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-vanished', codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.153.0', codex_pane_id: '%1', id: '$7',
        identity_token: creation_identity
      )
      replacement = original.dup
      replacement.id = '$8'
      replacement.codex_thread_id = 'thread-replacement'
      second_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          replacement.identity_token = keywords.fetch(:identity_token)
          replacement
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| replacement }
        define_method(:revalidate_session!) { |expected| expected }
        define_method(:reconcile_native_client!) { |_slug, expected, **_keywords| expected }
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
      session = DevSession::Tmux::Session.new(
        id: '$8', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-crash',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      crashing_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }
        define_method(:reconcile_native_client!) { |_slug, expected, **_keywords| expected }

        def mark_creation_journal_ready(slug, journal)
          super
          raise DevSession::Error, 'simulated crash before authority publication'
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

      assert_raises(DevSession::Error) do
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
      replay = DevSession::Runner.new(
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
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )

      assert_raises(DevSession::CommandError) do
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

      error = assert_raises(DevSession::Error) do
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

      creation_identity = JSON.parse(File.read(journal)).fetch('tmux_identity')
      retry_runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:, identity_token: creation_identity),
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
        assert_includes(
          command,
          "--portal-base-url #{DevSession::DEFAULT_PORTAL_BASE_URL}"
        )
        assert_includes(
          command,
          "--portal-url #{DevSession::DEFAULT_PORTAL_BASE_URL}/#{slug}/"
        )
      end
      assert_equal('thread-recovered', File.read(thread_marker).strip)
    end
  end

  def test_exclusive_browser_start_journals_before_tracking_files
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume after an early crash.\n")
      crashing_runner_class = Class.new(DevSession::Runner) do
        def ensure_tracking_files(slug, **)
          super
          raise DevSession::Error, 'simulated crash after tracking creation'
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

      assert_raises(DevSession::Error) do
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

      creation_identity = JSON.parse(File.read(journal)).fetch('tmux_identity')
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:, identity_token: creation_identity),
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
      crashing_runner_class = Class.new(DevSession::Runner) do
        def ensure_tracking_files(_slug, **)
          raise DevSession::Error, 'simulated crash before tracking creation'
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
      assert_raises(DevSession::Error) do
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

      error = assert_raises(DevSession::Error) do
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
    fixtures = Dir[File.expand_path('fixtures/portal-manifest-invalid-*.yml', __dir__)]
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
    Dir[File.expand_path('fixtures/runtime-authority-valid-*.json', __dir__)].each do |fixture|
      record = JSON.parse(File.read(fixture))
      record['workspace'] = runner.workspace
      runner.send(:validate_session_authority!, record, 'example', fixture)
    end
    Dir[File.expand_path('fixtures/runtime-authority-invalid-*.json', __dir__)].each do |fixture|
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
        'git@github.com:example-org/sample.git'
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
      assert_equal('example-org/sample', metadata['github'])
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
      remote = File.join(workspace, 'workspace-origin.git')
      assert_git_success('git', 'init', '--bare', remote)
      assert_git_success(
        'git', '-C', workspace, 'remote', 'add', 'origin',
        'git@github.com:example-org/example-workspace-workspace.git'
      )
      assert_git_success(
        'git', '-C', workspace, 'config',
        "url.#{remote}.insteadOf", 'git@github.com:example-org/example-workspace-workspace.git'
      )
      assert_git_success('git', '-C', workspace, 'push', 'origin', 'master')
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

      merge_registered_branches(workspace, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)

      refute(File.exist?(path))
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'archive', slug, 'portal.yml'))
      )
      repository = manifest.fetch('repositories').fetch(0)
      assert_equal('workspace', repository['name'])
      assert_equal('example-org/example-workspace-workspace', repository['github'])
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
        runner = DevSession::Runner.new(
          workspace:,
          command_runner:,
          tmux: NullTmux.new,
          out: StringIO.new,
          err: StringIO.new,
          today: TODAY
        )

        error = assert_raises(DevSession::Error) do
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

  def test_remove_discards_session_to_private_recovery_and_keeps_branch
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
      runner.delete('demo', as_is: false, force: false)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
      recovery = removal_recovery(workspace, slug)
      assert(File.exist?(File.join(recovery, 'work', 'plan.md')))
      assert_equal('removed', JSON.parse(File.read(File.join(recovery, 'recovery.json')))['state'])
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

  def test_remove_records_heads_in_recovered_tracking
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

      runner.delete('demo', as_is: false, force: false)
      recovery = removal_recovery(workspace, slug)
      manifest = YAML.safe_load(File.read(File.join(recovery, 'work', 'portal.yml')))
      assert_equal(expected_head, manifest.dig('repositories', 0, 'final_head_sha'))
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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
        on_kill: lambda {
          refute(File.exist?(path))
          assert(File.exist?(File.join(workspace, 'work', slug)))
        }
      )
      remove_runner = runner_for(workspace, tmux:)

      remove_runner.delete('demo', as_is: false, force: false)

      assert(tmux.killed)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
      assert(File.directory?(removal_recovery(workspace, slug)))
    end
  end

  def test_delete_all_is_rejected
    with_workspace do |workspace|
      err = StringIO.new
      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', 'demo', '--all'],
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_match(/invalid option: --all/, err.string)
    end
  end

  def test_delete_yes_bypass_is_rejected
    with_workspace do |workspace|
      runner_for(workspace).ensure_tracking_files('2026-06-06-demo')
      err = StringIO.new
      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is', '--yes'],
        input: StringIO.new,
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_match(/invalid option: --yes/, err.string)
      assert(File.directory?(File.join(workspace, 'work', '2026-06-06-demo')))
    end
  end

  def test_delete_requires_confirmation_in_noninteractive_cli
    with_workspace do |workspace|
      runner_for(workspace).ensure_tracking_files('2026-06-06-demo')
      err = StringIO.new

      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is'],
        input: StringIO.new,
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_includes(err.string, 'requires an interactive terminal')
      assert(File.directory?(File.join(workspace, 'work', '2026-06-06-demo')))
    end
  end

  def test_delete_uses_a_simple_interactive_confirmation
    with_workspace do |workspace|
      runner_for(workspace).ensure_tracking_files('2026-06-06-demo')
      err = StringIO.new

      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is'],
        input: TTYInput.new("no\n"),
        out: StringIO.new,
        err:,
        env: ENV.to_h.merge('XDG_STATE_HOME' => File.join(workspace, '.xdg-state'))
      ).run

      assert_equal(1, status)
      assert_includes(err.string, 'was not confirmed')
      assert(File.directory?(File.join(workspace, 'work', '2026-06-06-demo')))

      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is'],
        input: TTYInput.new("yes\n"),
        out: StringIO.new,
        err: StringIO.new,
        env: ENV.to_h.merge('XDG_STATE_HOME' => File.join(workspace, '.xdg-state'))
      ).run

      assert_equal(0, status)
      refute(File.exist?(File.join(workspace, 'work', '2026-06-06-demo')))
    end
  end

  def test_remove_discards_an_archived_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.mkdir_p(File.join(workspace, 'archive'))
      File.rename(File.join(workspace, 'work', slug), File.join(workspace, 'archive', slug))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(File.join(workspace, 'archive', slug)))
      assert(File.directory?(File.join(removal_recovery(workspace, slug), 'archive')))
    end
  end

  def test_remove_preserves_the_creation_journal
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Fix the session.\n")
      runner = runner_for(workspace)
      runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      runner.ensure_tracking_files(slug)

      runner.delete(slug, as_is: true, force: false)

      recovery = removal_recovery(workspace, slug)
      assert(File.file?(File.join(recovery, 'creation.json')))
      refute(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.creation.json")))
    end
  end

  def test_remove_force_refuses_unmanaged_worktree_entries
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      File.write(File.join(workspace, 'worktrees', slug, 'unmanaged.txt'), "keep me\n")

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'contains unmanaged entries')
      assert_equal(
        "keep me\n",
        File.read(File.join(workspace, 'worktrees', slug, 'unmanaged.txt'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_force_refuses_a_symlinked_worktree_entry
    with_workspace do |workspace|
      slug = '2026-06-06-symlinked-worktree'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      outside = File.join(workspace, 'outside-worktree')
      FileUtils.mkdir_p(outside)
      FileUtils.ln_s(outside, File.join(workspace, 'worktrees', slug, 'linked'))

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'contains unmanaged entries')
      assert(File.symlink?(File.join(workspace, 'worktrees', slug, 'linked')))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_force_refuses_a_worktree_from_a_foreign_repository
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-foreign-worktree'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      Dir.mktmpdir('external-dev-session-repository') do |external|
        FileUtils.mkdir_p(File.join(external, 'repos'))
        create_bare_repo(external, 'sample')
        bare = File.join(external, 'repos', 'sample.git')
        path = File.join(workspace, 'worktrees', slug, 'sample')
        assert_git_success('git', "--git-dir=#{bare}", 'worktree', 'add', path, 'master')

        error = assert_raises(DevSession::Error) do
          runner.delete(slug, as_is: true, force: true)
        end

        assert_includes(error.message, 'outside the canonical repository root')
        assert(File.directory?(path))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      end
    end
  end

  def test_remove_releases_both_development_cluster_types
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      helpers = File.join(workspace, 'helpers')
      log = File.join(workspace, 'cluster.log')
      FileUtils.mkdir_p(helpers)
      %w[alpha-devcluster beta-devcluster].each do |name|
        path = File.join(helpers, name)
        script = <<~'SH'
          #!/bin/sh
          if [ "$1" = cleanup-paths ]; then
            printf '%s\n' '{"schema":1,"paths":[]}'
            exit 0
          fi
          name=${0##*/}
          printf '%s:%s:%s\n' "$DEVCLUSTER_WORKSPACE" "$name" "$*" >> "$CLUSTER_LOG"
        SH
        File.write(path, script)
        File.chmod(0o755, path)
      end
      runner = runner_for(
        workspace,
        env: {'PATH' => helpers, 'CLUSTER_LOG' => log},
        alpha_cluster: File.join(helpers, 'alpha-devcluster'),
        beta_cluster: File.join(helpers, 'beta-devcluster')
      )
      runner.ensure_tracking_files(slug)

      runner.delete(slug, as_is: true, force: false)

      assert_equal(
        [
          "#{workspace}:alpha-devcluster:reset #{slug}",
          "#{workspace}:beta-devcluster:reset #{slug}"
        ],
        File.readlines(log, chomp: true)
      )
    end
  end

  def test_remove_retains_recovery_without_touching_worktrees_when_thread_retirement_fails
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      portal = File.join(workspace, 'portal')
      File.write(portal, "#!/bin/sh\nexit 19\n")
      File.chmod(0o755, portal)
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        portal_command: [portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.153.4'
      }
      File.write(manifest_path, YAML.dump(manifest))

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
      assert(File.directory?(File.join(workspace, 'work', slug)))
      recovery = removal_recovery(workspace, slug)
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retiring', journal.fetch('phase'))
      assert(File.file?(File.join(recovery, 'recovery.json')))
    end
  end

  def test_remove_seals_the_head_written_by_an_active_turn_before_cleanup
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-active-turn-head'
      creator = runner_for(workspace)
      creator.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        File.write(File.join(#{worktree.dump}, 'from-active-turn'), "changed\n")
        system(
          {'GIT_AUTHOR_NAME' => 'Test', 'GIT_AUTHOR_EMAIL' => 'test@example.invalid',
           'GIT_COMMITTER_NAME' => 'Test', 'GIT_COMMITTER_EMAIL' => 'test@example.invalid'},
          'git', '-C', #{worktree.dump}, 'add', 'from-active-turn'
        ) or abort 'unable to stage active-turn change'
        system(
          {'GIT_AUTHOR_NAME' => 'Test', 'GIT_AUTHOR_EMAIL' => 'test@example.invalid',
           'GIT_COMMITTER_NAME' => 'Test', 'GIT_COMMITTER_EMAIL' => 'test@example.invalid'},
          'git', '-C', #{worktree.dump}, 'commit', '-m', 'active turn change'
        ) or abort 'unable to commit active-turn change'
      RUBY
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {
        'thread_id' => 'thread-1', 'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.153.4'
      }
      File.write(manifest_path, YAML.dump(manifest))
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      runner.delete(slug, as_is: true, force: true)

      recovery = JSON.parse(File.read(File.join(removal_recovery(workspace, slug), 'recovery.json')))
      recorded = recovery.fetch('worktrees').fetch(0)
      assert_equal(
        git_capture_success(
          'git', "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
          'rev-parse', slug
        ).strip,
        recorded.fetch('head_sha')
      )
      assert_equal(false, recorded.fetch('dirty'))
      assert_equal(true, recorded.fetch('removed'))
    end
  end

  def test_remove_retry_refuses_a_worktree_added_after_deletion_started
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-added-worktree'
      creator = runner_for(workspace)
      creator.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      portal = File.join(workspace, 'portal')
      File.write(portal, "#!/bin/sh\nexit 19\n")
      File.chmod(0o755, portal)
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {
        'thread_id' => 'thread-1', 'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.153.4'
      }
      File.write(manifest_path, YAML.dump(manifest))
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, portal_command: [portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: true)
      end
      added = File.join(workspace, 'worktrees', slug, 'other')
      assert_git_success(
        'git', "--git-dir=#{File.join(workspace, 'repos', 'other.git')}",
        'worktree', 'add', '-b', slug, added, 'master'
      )
      File.write(portal, "#!/bin/sh\nexit 0\n")

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'worktrees were added after deletion started')
      assert(File.directory?(added))
      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
    end
  end

  def test_remove_retry_refuses_head_drift_after_inventory_is_sealed
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-sealed-head-drift'
      creator = runner_for(workspace)
      creator.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      helper = File.join(workspace, 'alpha-devcluster')
      marker = File.join(workspace, 'cluster-retried')
      File.write(helper, <<~SH)
        #!/bin/sh
        if [ "$1" = cleanup-paths ]; then
          printf '%s\n' '{"schema":1,"paths":[]}'
          exit 0
        fi
        if [ ! -e #{Shellwords.escape(marker)} ]; then
          : > #{Shellwords.escape(marker)}
          exit 19
        fi
      SH
      File.chmod(0o755, helper)
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, cluster_providers: { 'alpha' => helper },
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: true)
      end
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(worktree, 'later'), "changed\n")
      assert_git_success('git', '-C', worktree, 'add', 'later')
      assert_git_success(
        'git', '-C', worktree,
        '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
        'commit', '-m', 'later change'
      )

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'worktree head sha changed during deletion')
      assert(File.directory?(worktree))
    end
  end

  def test_remove_retry_proves_a_worktree_removed_before_its_journal_update
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-partial-worktree-removal'
      runner = runner_for(workspace)
      %w[sample other].each do |project|
        runner.worktree_add(
          slug, project, as_is: true, name: nil, branch: nil,
          base: 'master', fetch: false
        )
      end
      remove = runner.method(:remove_worktree_path)
      interrupted = false
      runner.define_singleton_method(:remove_worktree_path) do |*arguments, **options|
        remove.call(*arguments, **options)
        unless interrupted
          interrupted = true
          raise DevSession::Error, 'simulated interruption after worktree removal'
        end
      end

      assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal(0, journal.fetch('worktrees').count { |entry| entry.fetch('removed', false) })

      runner.delete(slug, as_is: true, force: false)

      recovery = JSON.parse(File.read(File.join(removal_recovery(workspace, slug), 'recovery.json')))
      assert(recovery.fetch('worktrees').all? { |entry| entry.fetch('removed') })
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_remove_rechecks_the_head_immediately_before_worktree_removal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-worktree-removal-race'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      remove = runner.method(:remove_worktree_path)
      changed = false
      runner.define_singleton_method(:remove_worktree_path) do |*arguments, **options|
        unless changed
          changed = true
          File.write(File.join(worktree, 'raced'), "changed\n")
          system('git', '-C', worktree, 'add', 'raced') or raise 'unable to stage race'
          system(
            'git', '-C', worktree,
            '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
            'commit', '-m', 'raced change'
          ) or raise 'unable to commit race'
        end
        remove.call(*arguments, **options)
      end

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'worktree head sha changed during cleanup')
      assert(File.directory?(worktree))
    end
  end

  def test_remove_discovers_a_thread_whose_start_result_was_lost
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal.rb')
      File.write(goal, "Create the session.\n")
      File.write(portal, <<~RUBY)
        case ARGV[1]
        when 'create'
          warn 'simulated loss after App Server committed thread/start'
          exit 19
        when 'retire'
          File.open(#{log.dump}, 'a') { |file| file.puts ARGV.join(' ') }
          abort 'cwd discovery unexpectedly supplied a thread ID' if ARGV.include?('--thread-id')
        else
          abort "unexpected portal action: \#{ARGV.join(' ')}"
        end
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      assert_raises(DevSession::CommandError) do
        runner.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_nil(manifest.dig('codex', 'thread_id'))

      runner.delete(slug, as_is: true, force: false)

      call = File.read(log)
      assert_includes(call, "thread retire --cwd #{File.join(workspace, 'work', slug)}")
      assert_includes(call, '--socket /run/test/codex.sock')
      refute_includes(call, '--thread-id')
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_can_upgrade_a_non_force_retirement_retry_to_force
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      calls = File.join(workspace, 'retire-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        File.open(#{calls.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        unless ARGV.include?('--force')
          warn 'Codex thread still has an active turn'
          exit 19
        end
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(
          slug,
          workspace:,
          codex_thread_id: 'thread-1',
          codex_socket_path: '/run/test/codex.sock'
        ),
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.ensure_tracking_files(slug)

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal(false, journal.fetch('force'))
      assert_equal('thread_retiring', journal.fetch('phase'))

      runner.delete(slug, as_is: true, force: true)

      lines = File.readlines(calls, chomp: true)
      refute_includes(lines.fetch(0), '--force')
      assert_includes(lines.fetch(1), '--force')
      recovery = JSON.parse(File.read(File.join(removal_recovery(workspace, slug), 'recovery.json')))
      assert_equal(true, recovery.fetch('force'))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      state_home = File.join(workspace, 'work', slug, 'private-state')
      runner = runner_for(workspace, env: {'XDG_STATE_HOME' => state_home})
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(state_home))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_the_creation_journal
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Create the session.\n")
      creator = runner_for(workspace)
      creator.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      creator.ensure_tracking_files(slug)
      journal = creator.send(:creation_journal_file, slug)
      runner = runner_for(workspace, env: {'XDG_STATE_HOME' => journal})

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.file?(journal))
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_force_rejects_recovery_storage_nested_in_a_worktree
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      creator = runner_for(workspace)
      creator.worktree_add(
        'demo', 'sample', as_is: false, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      state_home = File.join(worktree, 'private-state')
      runner = runner_for(workspace, env: {'XDG_STATE_HOME' => state_home})

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(worktree))
      refute(File.exist?(state_home))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_absent_cluster_state
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cluster = File.join(workspace, '.dev-clusters', 'alpha', 'clusters', slug)
      state_home = File.join(cluster, 'private-state')
      helper = cleanup_contract_helper(workspace, 'alpha', [cluster])
      runner = runner_for(
        workspace,
        env: {'XDG_STATE_HOME' => state_home},
        alpha_cluster: helper
      )
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(state_home))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_cluster_socket
    with_workspace do |workspace|
      {
        'alpha' => 'example-devcluster',
        'beta' => 'beta-devcluster'
      }.each do |kind, prefix|
        slug = "2026-06-06-demo-#{kind}"
        digest = Digest::SHA256.hexdigest("#{workspace}\0#{slug}")[0, 12]
        socket = File.join('/tmp', "#{prefix}-#{digest}")
        state_home = File.join(socket, 'private-state')
        helper = cleanup_contract_helper(workspace, kind, [socket])
        runner = runner_for(
          workspace,
          env: {'XDG_STATE_HOME' => state_home},
          alpha_cluster: kind == 'alpha' ? helper : nil,
          beta_cluster: kind == 'beta' ? helper : nil
        )
        runner.ensure_tracking_files(slug)

        error = assert_raises(DevSession::Error) do
          runner.delete(slug, as_is: true, force: false)
        end

        assert_includes(error.message, 'recovery root is inside session state')
        assert(File.directory?(File.join(workspace, 'work', slug)))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      end
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_legacy_cluster_socket
    with_workspace do |workspace|
      slug = '2026-08-18-alpha-password-reset'
      digest = Digest::SHA256.hexdigest(slug)[0, 12]
      socket = File.join('/tmp', "example-devcluster-#{digest}")
      state_home = File.join(socket, 'private-removal-state')
      helper = cleanup_contract_helper(workspace, 'alpha', [socket])
      runner = runner_for(
        workspace,
        env: {'XDG_STATE_HOME' => state_home},
        alpha_cluster: helper
      )
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_reconciles_an_archive_before_its_phase_update
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      calls = File.join(workspace, 'retire-calls')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~RUBY)
        File.open(#{calls.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        abort 'retirement lost its thread identity' unless ARGV.include?('--thread-id')
      RUBY
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock'
      )
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.ensure_tracking_files(slug)
      advance = runner.method(:advance_removal!)
      interrupted = false
      runner.define_singleton_method(:advance_removal!) do |current_slug, removal, phase|
        if phase == 'thread_retired' && !interrupted
          interrupted = true
          raise DevSession::Error, 'simulated interruption after thread archive'
        end
        advance.call(current_slug, removal, phase)
      end

      assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retiring', journal.fetch('phase'))
      assert(tmux.quiesced)

      runner.delete(slug, as_is: true, force: false)

      assert_equal(2, File.readlines(calls).length)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_refuses_to_orphan_a_known_thread_when_runtime_is_unavailable
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {'thread_id' => 'thread-1'}
      File.write(manifest_path, YAML.dump(manifest))

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'workspace-portal is required')
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retiring', journal.fetch('phase'))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_retries_after_cluster_release_failure
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      helper = File.join(workspace, 'alpha-devcluster')
      marker = File.join(workspace, 'cluster-retried')
      File.write(helper, <<~SH)
        #!/bin/sh
        if [ "$1" = cleanup-paths ]; then
          printf '%s\n' '{"schema":1,"paths":[]}'
          exit 0
        fi
        if [ ! -e #{Shellwords.escape(marker)} ]; then
          : > #{Shellwords.escape(marker)}
          exit 19
        fi
      SH
      File.chmod(0o755, helper)
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        cluster_providers: { 'alpha' => helper },
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.ensure_tracking_files(slug)

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retired', journal.fetch('phase'))
      assert(File.directory?(File.join(workspace, 'work', slug)))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_passes_its_exclusive_session_lock_to_cluster_reset
    with_workspace do |workspace|
      slug = '2026-06-06-cluster-lock-owner'
      cluster = File.join(workspace, '.dev-clusters', 'alpha', 'clusters', slug)
      helper = File.join(workspace, 'alpha-devcluster')
      File.write(helper, <<~SH)
        #!/bin/sh
        if [ "$1" = cleanup-paths ]; then
          printf '%s\n' #{Shellwords.escape(JSON.generate('schema' => 1, 'paths' => [cluster]))}
          exit 0
        fi
        test "$1" = reset
        test "$DEV_SESSION_LIFECYCLE_OPERATION" = delete
        test -n "$DEV_SESSION_LIFECYCLE_LOCK_FD"
        test -n "$DEV_SESSION_LIFECYCLE_LOCK_PATH"
        rm -rf -- #{Shellwords.escape(cluster)}
      SH
      File.chmod(0o755, helper)
      runner = runner_for(
        workspace,
        alpha_cluster: helper
      )
      runner.ensure_tracking_files(slug)
      FileUtils.mkdir_p(cluster)

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(cluster))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_preflights_legacy_identity_before_creating_a_journal
    with_workspace do |workspace|
      slug = '2026-06-06-remove-legacy-identity'
      authority_dir = File.join(workspace, 'authority')
      tmux = RefusedIdentityInitializationTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(tmux.identity_initialization_attempted)
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_a_renamed_authority_session_before_creating_a_journal
    with_workspace do |workspace|
      slug = '2026-06-06-remove-renamed-session'
      authority_dir = File.join(workspace, 'authority')
      tmux = RenamedManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      session = DevSession::Tmux::Session.new(
        id: '$11', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, session, state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_resume_upgrades_a_legacy_identity_before_continuing
    with_workspace do |workspace|
      slug = '2026-06-06-remove-resume-legacy'
      authority_dir = File.join(workspace, 'authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      runner.send(:prepare_removal!, slug, force: false)

      runner.delete(slug, as_is: true, force: false)

      assert(tmux.killed)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_removal_journal_is_bound_to_the_portal_operation_identity
    with_workspace do |workspace|
      slug = '2026-06-06-remove-operation-identity'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      operation_id = 'a' * 64

      removal = runner.send(
        :prepare_removal!, slug, force: false, operation_id:
      )
      assert_equal(operation_id, removal.fetch('operation_id'))
      assert_equal(
        operation_id,
        runner.send(:load_removal_journal, slug).fetch('operation_id')
      )

      error = assert_raises(DevSession::Error) do
        runner.send(
          :prepare_removal!, slug, force: false, operation_id: 'b' * 64
        )
      end
      assert_includes(error.message, 'session deletion operation changed')
    end
  end

  def test_archive_journal_is_bound_to_the_portal_operation_identity
    with_workspace do |workspace|
      slug = '2026-06-06-archive-operation-identity'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      operation_id = 'a' * 64
      plan = runner.send(:prepare_cleanup, slug, force: false)

      journal = runner.send(
        :prepare_archive_journal!,
        slug,
        'complete',
        {},
        plan,
        operation_id:
      )
      assert_equal(operation_id, journal.fetch('operation_id'))
      assert_equal(
        operation_id,
        runner.send(:load_archive_journal, slug).fetch('operation_id')
      )

      error = assert_raises(DevSession::Error) do
        runner.archive(
          slug,
          as_is: true,
          operation_id: 'b' * 64
        )
      end
      assert_includes(error.message, 'session archive operation changed')
    end
  end

  def test_revive_journal_is_bound_to_the_portal_operation_identity
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-operation-identity'
      runner = archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      operation_id = 'a' * 64

      journal = runner.send(
        :prepare_revive_journal!,
        slug,
        'complete',
        operation_id:
      )
      assert_equal(operation_id, journal.fetch('operation_id'))
      assert_equal(
        operation_id,
        runner.send(:load_revive_journal, slug).fetch('operation_id')
      )

      error = assert_raises(DevSession::Error) do
        runner.revive(
          slug,
          as_is: true,
          operation_id: 'b' * 64
        )
      end
      assert_includes(error.message, 'session revive operation changed')
    end
  end

  def test_remove_retries_after_tmux_was_killed
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = KillThenFailOnceTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      authority_dir = File.join(workspace, 'authority')
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      assert_includes(error.message, 'after tmux removal')
      assert(tmux.killed)
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('worktrees_removed', journal.fetch('phase'))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_recovers_a_tracking_move_before_its_phase_update
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      advance = runner.method(:advance_removal!)
      interrupted = false
      runner.define_singleton_method(:advance_removal!) do |current_slug, removal, phase|
        if phase == 'tracking_preserved' && !interrupted
          interrupted = true
          raise DevSession::Error, 'simulated interruption after tracking move'
        end
        advance.call(current_slug, removal, phase)
      end

      assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      recovery = removal_recovery(workspace, slug)
      assert(File.directory?(File.join(recovery, 'work')))
      refute(File.exist?(File.join(workspace, 'work', slug)))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('removed', JSON.parse(File.read(File.join(recovery, 'recovery.json'))).fetch('state'))
    end
  end

  def test_remove_commits_tracked_active_and_archived_sessions
    %w[work archive].each do |tracking_kind|
      with_workspace do |workspace|
        slug = "2026-06-06-#{tracking_kind}"
        runner = runner_for(workspace)
        runner.ensure_tracking_files(slug)
        commit_tracking(workspace, slug, lifecycle: 'active')
        if tracking_kind == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(File.join(workspace, 'work', slug), File.join(workspace, 'archive', slug))
          commit_archive_move(workspace, slug)
        end
        configure_workspace_origin(workspace)
        unrelated = File.join(workspace, 'unrelated.txt')
        File.write(unrelated, "keep staged\n")
        assert_git_success('git', '-C', workspace, 'add', 'unrelated.txt')

        runner.delete(slug, as_is: true, force: false)

        relative = File.join(tracking_kind, slug)
        assert_equal('', git_capture_success('git', '-C', workspace, 'ls-files', '--', relative))
        assert_equal("workspace: delete #{slug}", git_capture_success(
          'git', '-C', workspace, 'log', '-1', '--format=%s'
        ).strip)
        assert_equal('A  unrelated.txt', git_capture_success(
          'git', '-C', workspace, 'status', '--short', '--', 'unrelated.txt'
        ).strip)
      end
    end
  end

  def test_remove_retries_a_failed_tracking_deletion_commit
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      marker = File.join(workspace, '.git', 'remove-hook-retried')
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, <<~SH)
        #!/bin/sh
        if [ ! -e #{Shellwords.escape(marker)} ]; then
          : > #{Shellwords.escape(marker)}
          exit 1
        fi
      SH
      File.chmod(0o755, hook)

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', journal.fetch('phase'))
      refute(File.exist?(File.join(workspace, 'work', slug)))

      [
        -> { runner.start(slug, as_is: true, new: false, attach: false, run_codex: false) },
        -> { runner.revive(slug, as_is: true) },
        -> {
          runner.worktree_add(
            slug, 'sample', as_is: true, name: nil, branch: nil,
            base: nil, fetch: false
          )
        }
      ].each do |operation|
        error = assert_raises(DevSession::Error, &operation)
      assert_includes(error.message, 'deletion is unfinished')
      end

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('', git_capture_success(
        'git', '-C', workspace, 'ls-files', '--', File.join('work', slug)
      ))
    end
  end

  def test_remove_keeps_its_journal_when_a_successful_hook_recreates_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-hook-recreates-tracking'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      recreated = File.join(workspace, 'work', slug)
      File.write(hook, <<~SH)
        #!/bin/sh
        mkdir -p #{Shellwords.escape(recreated)}
        printf 'recreated by hook\n' > #{Shellwords.escape(File.join(recreated, 'unexpected.txt'))}
      SH
      File.chmod(0o755, hook)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'tracking reappeared')
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', journal.fetch('phase'))
      assert(File.file?(File.join(recreated, 'unexpected.txt')))
      assert_equal('', git_capture_success(
        'git', '-C', workspace, 'ls-files', '--', File.join('work', slug)
      ))

      retry_error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      assert_includes(retry_error.message, 'tracking reappeared')
      retry_journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', retry_journal.fetch('phase'))
    end
  end

  def test_remove_rechecks_remote_master_immediately_before_deletion_commit
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      remote = File.join(workspace, '.git', 'test-origin.git')
      commit = runner.method(:commit_removed_tracking!)
      advanced = false
      runner.define_singleton_method(:commit_removed_tracking!) do |current_slug, removal|
        unless advanced
          advanced = true
          Dir.mktmpdir('dev-session-remote-advance') do |checkout|
            system('git', 'clone', remote, checkout, out: File::NULL, err: File::NULL) || raise('clone failed')
            system('git', '-C', checkout, 'config', 'user.email', 'test@example.invalid') || raise('config failed')
            system('git', '-C', checkout, 'config', 'user.name', 'Test User') || raise('config failed')
            File.write(File.join(checkout, 'remote.txt'), "advanced\n")
            system('git', '-C', checkout, 'add', 'remote.txt') || raise('add failed')
            system('git', '-C', checkout, 'commit', '-m', 'advance remote', out: File::NULL) || raise('commit failed')
            system('git', '-C', checkout, 'push', 'origin', 'master', out: File::NULL) || raise('push failed')
          end
        end
        commit.call(current_slug, removal)
      end

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'workspace master advanced')
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', journal.fetch('phase'))
      assert_equal('start initiative', git_capture_success(
        'git', '-C', workspace, 'log', '-1', '--format=%s'
      ).strip)
    end
  end

  def test_remove_uses_verified_worktrees_missing_from_the_portal_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-unregistered-worktree'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['repositories'] = []
      File.write(manifest_path, YAML.dump(manifest))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(path))
      recovery = JSON.parse(File.read(File.join(
        removal_recovery(workspace, slug), 'recovery.json'
      )))
      assert_equal(
        [{
          'name' => 'sample',
          'project' => 'sample',
          'path' => path,
          'git_common_dir' => File.join(workspace, 'repos', 'sample.git'),
          'branch' => slug,
          'head_sha' => head,
          'dirty' => false,
          'removed' => true
        }],
        recovery.fetch('worktrees')
      )
      assert_git_success(
        'git', "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref', '--verify', "refs/heads/#{slug}"
      )
    end
  end

  def test_remove_ignores_a_stale_portal_worktree_identity
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-stale-worktree-registration'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest.fetch('repositories').fetch(0)['project'] = 'other'
      File.write(manifest_path, YAML.dump(manifest))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      recovery = JSON.parse(File.read(File.join(
        removal_recovery(workspace, slug), 'recovery.json'
      )))
      assert_equal('sample', recovery.fetch('worktrees').fetch(0).fetch('project'))
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

      error = assert_raises(DevSession::Error) do
        runner.delete('demo', as_is: false, force: false)
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

      runner.delete('demo', as_is: false, force: true)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
      recovery = removal_recovery(workspace, slug)
      assert(File.directory?(recovery))
      metadata = JSON.parse(File.read(File.join(recovery, 'recovery.json')))
      assert_equal(true, metadata.fetch('worktrees').fetch(0).fetch('dirty'))
    end
  end

  def test_remove_kills_managed_tmux_session
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
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      assert(tmux_session_exists?(socket, slug))

      runner.delete('demo', as_is: false, force: false)

      refute(tmux_session_exists?(socket, slug))
      refute(File.exist?(File.join(workspace, 'work', slug)))
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
      cluster_helper = cleanup_contract_helper(workspace, 'empty-cluster', [])

      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        cluster_providers: { 'alpha' => cluster_helper, 'beta' => cluster_helper },
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      error = assert_raises(DevSession::Error) do
        runner.delete('demo', as_is: false, force: false)
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
      merge_registered_branches(workspace, slug)
      finalize_core(runner, 'demo', as_is: false)

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

      error = assert_raises(DevSession::Error) do
        runner.stop(slug, as_is: true)
      end
      assert_match(/must be committed before stopping/, error.message)
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
        finalize_core(runner, slug, as_is: true)
        FileUtils.mkdir_p(File.join(workspace, 'work', slug))

        error = assert_raises(DevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.delete(slug, as_is: true, force: false)
          end
        end

        assert_match(/active and archived tracking both exist/, error.message)
        refute(tmux.killed)
      end

      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        tmux = ManagedTmux.new(slug, workspace:)
        runner = runner_for(workspace, tmux:)

        error = assert_raises(DevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.delete(slug, as_is: true, force: false)
          end
        end

        assert_match(/session tracking is missing/, error.message)
        refute(tmux.killed)
      end
    end
  end

  def test_session_closing_rejects_invalid_active_tracking
    cases = %i[symlink file empty_directory].map { |kind| [:stop, kind] }
    cases += %i[symlink file].map { |kind| [:remove, kind] }
    cases.each do |operation, kind|
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
        error = assert_raises(DevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.delete(slug, as_is: true, force: false)
          end
        end

        assert_match(/work directory|session tracking|missing tracking files/, error.message)
        refute(tmux.killed)
      end
    end
  end

  def test_remove_accepts_incomplete_empty_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      tmux = ManagedTmux.new(slug, workspace:)

      runner_for(workspace, tmux:).delete(slug, as_is: true, force: false)

      assert(tmux.killed)
      refute(File.exist?(File.join(workspace, 'work', slug)))
      assert(File.directory?(File.join(removal_recovery(workspace, slug), 'work')))
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
      error = assert_raises(DevSession::Error) do
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

      finalize_core(runner, 'demo', as_is: false)

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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
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
        error = assert_raises(DevSession::Error) do
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
        assert_raises(DevSession::Error, File.basename(fixture)) do
          runner_for(workspace).send(:lifecycle_state, File.binread(fixture))
        end
      end
    end
  end

  def test_lifecycle_parser_rejects_invalid_utf8_and_oversized_input
    with_workspace do |workspace|
      runner = runner_for(workspace)
      assert_raises(DevSession::Error) do
        runner.send(:lifecycle_state, "---\nlifecycle: active\n---\n\xff".b)
      end
      assert_raises(DevSession::Error) do
        runner.send(
          :lifecycle_state,
          "---\nlifecycle: active\n---\n" + ('x' * DevSession::TRACKING_MAX_SIZE)
        )
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
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
      finalize_core(runner, slug, as_is: true)
      state = File.join(workspace, 'archive', slug, 'state.md')
      content = File.read(state).sub('lifecycle: complete', 'lifecycle: active')
      File.write(state, state_with_body_lifecycle(content, 'complete'))
      commit_archive_move(workspace, slug)

      error = assert_raises(DevSession::Error) do
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner_for(workspace, tmux:), 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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
        runner = DevSession::Runner.new(
          workspace:,
          command_runner:,
          tmux: NullTmux.new,
          out: StringIO.new,
          err: StringIO.new,
          today: TODAY
        )

        assert_raises(DevSession::Error) do
          finalize_core(runner, 'demo', as_is: false)
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

  def test_atomic_archive_move_syncs_both_parent_directories
    with_workspace do |workspace|
      source = File.join(workspace, 'work', '2026-06-06-demo')
      destination = File.join(workspace, 'archive', '2026-06-06-demo')
      FileUtils.mkdir_p(source)
      FileUtils.mkdir_p(File.dirname(destination))
      runner = runner_for(workspace)
      synced = []
      runner.define_singleton_method(:fsync_directory) { |path| synced << path }

      runner.send(:atomic_archive_move!, source, destination)

      assert_equal(
        [File.join(workspace, 'work'), File.join(workspace, 'archive')],
        synced
      )
      refute(File.exist?(source))
      assert(File.directory?(destination))
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

        raise DevSession::Error, 'atomic move options unavailable'
      end
      runner = DevSession::Runner.new(
        workspace:,
        command_runner:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
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
      runner = DevSession::Runner.new(
        workspace:,
        command_runner:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      finalize_core(runner, slug, as_is: true)

      assert_equal(2, move_commands.length)
      move_commands.each do |argv|
        assert_equal(
          ['mv', *DevSession::ARCHIVE_MOVE_OPTIONS],
          argv.take(DevSession::ARCHIVE_MOVE_OPTIONS.length + 1)
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

        error = assert_raises(DevSession::Error) do
          runner.delete(slug, as_is: true, force: false)
        end
        assert_match(/another dev-session command/, error.message)

        runner.delete(other_slug, as_is: true, force: false)
        refute(File.exist?(File.join(workspace, 'worktrees', other_slug)))
      end

      runner.delete(slug, as_is: true, force: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
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

        error = assert_raises(DevSession::Error) do
          finalize_core(runner, slug, as_is: true)
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

      error = assert_raises(DevSession::Error) do
        finalize_core(
          runner_for(workspace, tmux: UnmanagedTmux.new(slug)),
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

  def test_exclusive_replay_publishes_authority_before_consuming_start_journal
    with_workspace do |workspace|
      slug = '2026-06-06-restart'
      goal = File.join(workspace, 'goal.txt')
      authority_dir = File.join(workspace, 'authority')
      File.write(goal, "Replay this completed request.\n")
      setup = runner_for(workspace, authority_dir:)
      creation = setup.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: false,
        model: nil,
        effort: nil
      )
      setup.ensure_tracking_files(slug)
      setup.send(:seed_goal, slug, goal)
      manifest = setup.send(:ensure_portal_manifest, slug, creation_journal: creation)
      manifest['creation']['state'] = 'ready'
      manifest['creation']['initial_goal_sent'] = true
      manifest['creation'].delete('initial_goal_attempted')
      manifest['schema'] = 1
      setup.send(:write_portal_manifest, slug, manifest)
      creation_identity = creation.fetch('tmux_identity')
      setup.send(:mark_creation_journal_ready, slug, creation)
      runtime = setup.send(
        :prepare_start_journal!,
        slug,
        preferred_identity: creation_identity
      )
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/test/tmux.sock',
        identity_token: runtime.fetch('tmux_identity'),
        id: '$12'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)

      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: false,
        goal_file: goal,
        exclusive: true
      )

      authority = JSON.parse(File.read(runner.send(:authority_file, slug)))
      assert_equal(runtime.fetch('tmux_identity'), authority.fetch('tmux_identity'))
      refute(File.exist?(setup.send(:start_journal_file, slug)))
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

        add_error = assert_raises(DevSession::Error) do
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

        remove_error = assert_raises(DevSession::Error) do
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

      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/session changed during operation/, error.message)
      refute(tmux.kill_attempted)
    end
  end

  def test_stop_keeps_authority_when_identity_changes_at_the_kill_boundary
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ReplacedDuringConditionalKillTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.stop(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(tmux.conditional_kill_attempted)
      refute(tmux.killed)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_stop_upgrades_a_live_legacy_authority_before_retirement
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      authority_path = File.join(authority_dir, "#{slug}.json")
      refute(JSON.parse(File.read(authority_path)).key?('tmux_identity'))

      runner.stop(slug, as_is: true)

      assert(tmux.killed)
      refute(File.exist?(authority_path))
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
      runner = DevSession::Runner.new(
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
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::Error) do
        runner.stop(slug, as_is: true)
      end
      assert_includes(error.message, 'unable to restore terminal Codex client')
      refute(tmux.killed)
      assert(tmux.quiesced)
      assert_empty(tmux.sent_commands)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))

      idle_runner = DevSession::Runner.new(
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

  def test_idle_check_ignores_a_manifest_without_current_socket_provenance
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
      RUBY
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock', codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = { 'thread_id' => 'thread-legacy' }
      runner.send(:write_portal_manifest, slug, manifest)

      runner.send(:ensure_portal_thread_idle!, slug, nil)

      refute(File.exist?(log))
    end
  end

  def test_idle_check_ignores_a_manifest_from_another_runtime
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
      RUBY
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock', codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-old',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      runner.send(:ensure_portal_thread_idle!, slug, nil)

      refute(File.exist?(log))
    end
  end

  def test_stop_quiesces_terminal_before_the_authoritative_idle_check
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      active = File.join(workspace, 'turn-active')
      portal = File.join(workspace, 'portal')
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        #!/usr/bin/env ruby
        if ARGV.include?('require-idle') && File.exist?(#{active.dump})
          abort 'turn became active while terminal was quiesced'
        end
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
      runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex,
        tmux:,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::CommandError) do
        runner.stop(slug, as_is: true)
      end
      assert_includes(error.message, 'turn became active')
      refute(tmux.killed)
      refute(tmux.quiesced, 'terminal Codex client was not restored')
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_quiesce_does_not_restore_a_client_that_was_already_stopped
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        codex_thread_id: 'thread-1',
        codex_pane_id: '%1',
        pane_current_command: File.basename(ENV.fetch('SHELL', '/bin/sh'))
      )
      runner = runner_for(workspace, tmux:)

      assert_nil(runner.send(:quiesce_native_client!, slug, tmux.session(slug)))
      refute(tmux.quiesced)
      assert_empty(tmux.sent_commands)
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
      runner = DevSession::Runner.new(
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
      assert_raises(DevSession::Error) do
        runner.recover_stale(slug, as_is: true)
      end

      tmux.run('kill-session', '-t', '$7:')
      runner.recover_stale(slug, as_is: true)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_recover_stale_accepts_an_id_reused_with_a_different_identity
    with_workspace do |workspace|
      slug = '2026-06-06-stale-reused-id'
      authority_dir = File.join(workspace, 'runtime-authority')
      replacement = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock', id: '$7',
        identity_token: 'b' * 64
      )
      runner = runner_for(workspace, tmux: replacement, authority_dir:)
      runner.ensure_tracking_files(slug)
      original = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test/tmux.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, original, state: 'ready')

      runner.recover_stale(slug, as_is: true)

      refute(replacement.killed)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_remove_does_not_kill_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner_for(workspace).ensure_tracking_files(slug)
      tmux = ReplacedTmux.new(slug, workspace:)

      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).delete(slug, as_is: true, force: false)
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
      runner = DevSession::Runner.new(
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
        other_runner = DevSession::Runner.new(
          workspace: other_workspace,
          tmux_socket: socket,
          out: other_out,
          err: StringIO.new,
          today: TODAY,
          env: { 'TMUX' => 'socket', 'TMUX_PANE' => pane }
        )

        current_error = assert_raises(DevSession::Error) do
          other_runner.current
        end
        assert_match(/not managed by this workspace/, current_error.message)

        other_runner.list
        assert_equal('', other_out.string)
        lookup_error = assert_raises(DevSession::Error) do
          other_runner.lookup_slug('demo', as_is: false)
        end
        assert_match(/no slug found/, lookup_error.message)

        error = assert_raises(DevSession::Error) do
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
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        tmux_socket: '/run/workspace-tmux/tmux.sock',
        authority_dir: File.join(workspace, 'runtime-authority'),
        codex_socket: '/run/workspace-codex/app-server.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        portal_command: ['/run/current-system/sw/bin/workspace-portal'],
        cluster_providers: { 'alpha' => RbConfig.ruby, 'beta' => RbConfig.ruby },
        require_runtime: true,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      finalize_core(runner, slug, as_is: true)

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
      runner = DevSession::Runner.new(
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

      ordinary_runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      finalize_core(ordinary_runner, 'demo', as_is: false)

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
      runner = DevSession::Runner.new(
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
    codex_socket = '/run/dev-workspace-codex/app-server.sock'

    with_workspace do |workspace|
      codex_executable = File.join(workspace, 'codex')
      codex_log = File.join(workspace, 'codex.log')
      File.write(codex_executable, <<~SH)
        #!/bin/sh
        [ "$1" = --version ] && { echo 'codex-cli 0.152.1'; exit 0; }
        printf '%s\n' "$@" > #{(codex_log + '.tmp').dump}
        mv #{(codex_log + '.tmp').dump} #{codex_log.dump}
        sleep 2
      SH
      File.chmod(0o755, codex_executable)
      portal_log = File.join(workspace, 'portal.log')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Investigate the reported issue.\n")
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{portal_log.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        if ARGV[0, 2] == ['thread', 'create']
          puts JSON.generate(threadId: 'thread-123')
        end
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_socket:,
        codex_version: '0.152.1',
        codex_command: codex_executable,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal
      )

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-123', manifest.dig('codex', 'thread_id'))
      assert_equal(codex_socket, manifest.dig('codex', 'socket_path'))
      assert_equal('0.152.1', manifest.dig('codex', 'client_version'))
      refute(manifest.key?('tmux'))
      wait_for_file(codex_log)
      codex_arguments = File.readlines(codex_log, chomp: true)
      assert_includes(codex_arguments, '--remote')
      assert_includes(codex_arguments, "unix://#{codex_socket}")
      assert_includes(codex_arguments, 'resume')
      assert_includes(codex_arguments, 'thread-123')
      portal_commands = File.readlines(portal_log, chomp: true)
      assert(portal_commands[0].start_with?('thread create '))
      assert(portal_commands[1].start_with?('thread set-name '))
      assert(portal_commands[2].start_with?('thread ensure-initial '))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_codex_provenance_requires_the_configured_executable_version
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Investigate the reported issue.\n")
      codex_executable = File.join(workspace, 'codex')
      File.write(codex_executable, "#!/bin/sh\necho 'codex-cli 0.151.0'\n")
      File.chmod(0o755, codex_executable)
      session = DevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
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

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal
        )
      end
      assert_includes(error.message, 'does not report configured version')
    end
  end

  def test_tmux_start_and_sync_manage_only_worktree_windows
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      create_bare_repo(workspace, 'alpha')
      repository = File.join(workspace, 'repos', 'alpha.git')
      master = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success('git', "--git-dir=#{repository}", 'branch', slug, 'master')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'update-ref',
        'refs/remotes/origin/master', master
      )
      assert_git_success(
        'git', "--git-dir=#{repository}", 'symbolic-ref',
        'refs/remotes/origin/HEAD', 'refs/remotes/origin/master'
      )
      worktree = File.join(workspace, 'worktrees', slug, 'alpha')
      FileUtils.mkdir_p(File.dirname(worktree))
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', worktree, slug
      )

      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)

      session_env = tmux_capture(socket, 'show-environment', '-t', slug)
      assert_includes(session_env, "#{DevSession::ENV_SLUG}=#{slug}\n")
      assert_includes(
        session_env,
        "#{DevSession::ENV_WORKSPACE}=#{workspace}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_WORK_DIR}=#{File.join(workspace, 'work', slug)}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_WORKTREES_DIR}=#{File.join(workspace, 'worktrees', slug)}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_PORTAL_BASE_URL}=#{DevSession::DEFAULT_PORTAL_BASE_URL}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_PORTAL_URL}=#{DevSession::DEFAULT_PORTAL_BASE_URL}/#{slug}/\n"
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
        '#{window_name}:#{@dev_session_window}'
      ).lines.map(&:chomp)

      assert_includes(windows, 'alpha:worktree')

      probe = File.join(workspace, 'alpha-env.txt')
      command = "printf '%s\\n' \"$#{DevSession::ENV_SLUG}\" > #{Shellwords.escape(probe)}"
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
      runner = DevSession::Runner.new(
        workspace:,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        out: StringIO.new,
        err: StringIO.new,
        env: {}
      )
      session = DevSession::Tmux::Session.new(
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.152.1'
      )

      assert(runner.send(:session_codex_provenance_matches?, session, 'thread-1'))
      session.codex_socket_path = '/run/workspace/other.sock'
      refute(runner.send(:session_codex_provenance_matches?, session, 'thread-1'))
    end
  end

  def test_start_rejects_a_ready_thread_from_another_app_server_runtime
    with_workspace do |workspace|
      slug = '2026-06-06-migrated'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(slug)
      manifest = setup.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      setup.send(:write_portal_manifest, slug, manifest)
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      session = DevSession::Tmux::Session.new(
        id: '$migrated', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/new/tmux.sock', codex_thread_id: 'thread-1',
        codex_socket_path: '/run/new/app-server.sock',
        codex_client_version: '0.153.2', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) { |*_args, **_options| session }
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/new/app-server.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [
          RbConfig.ruby,
          '-e',
          "require 'json'; puts JSON.generate(threadId: 'thread-1') if ARGV[0, 2] == ['thread', 'create']"
        ],
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: true)
      end

      assert_includes(error.message, 'belongs to another runtime')
      retained = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-1', retained.dig('codex', 'thread_id'))
      assert_equal('/run/old/app-server.sock', retained.dig('codex', 'socket_path'))
      assert_equal('0.151.0', retained.dig('codex', 'client_version'))
    end
  end

  def test_exclusive_replay_rejects_a_ready_thread_from_another_runtime
    with_workspace do |workspace|
      slug = '2026-06-06-old-runtime'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue the original request.\n")
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
      setup.ensure_tracking_files(slug)
      manifest = setup.send(:ensure_portal_manifest, slug, creation_journal: journal)
      manifest['schema'] = 1
      manifest['creation']['state'] = 'ready'
      manifest['creation']['initial_goal_sent'] = true
      manifest['creation'].delete('initial_goal_attempted')
      manifest['codex'] = {
        'thread_id' => 'thread-old',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      setup.send(:write_portal_manifest, slug, manifest)
      setup.send(:mark_creation_journal_ready, slug, journal)
      called = File.join(workspace, 'portal-called')
      portal = [RbConfig.ruby, '-e', "File.write(#{called.dump}, 'called'); exit 1"]
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: portal,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(DevSession::Error) do
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

      assert_includes(error.message, 'belongs to another runtime')
      refute(File.exist?(called))
      retained = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('/run/old/app-server.sock', retained.dig('codex', 'socket_path'))
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
      runner = DevSession::Runner.new(
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
      running_runner = DevSession::Runner.new(
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

  def test_terminal_reconciliation_refuses_an_incomplete_creation
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      portal_log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(portal, <<~RUBY)
        File.write(#{portal_log.dump}, ARGV.join(' '))
      RUBY
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-creating',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2',
        pane_current_command: 'sh'
      )
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['schema'] = 2
      manifest['codex'] = {
        'thread_id' => 'thread-creating',
        'socket_path' => '/run/workspace/codex.sock',
        'client_version' => '0.153.2'
      }
      manifest['creation'] = {
        'state' => 'creating',
        'initial_goal_sent' => false,
        'initial_goal_attempted' => true,
        'goal_sha256' => Digest::SHA256.hexdigest('initial request')
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::Error) do
        runner.send(:reconcile_native_client!, slug, tmux.session(slug))
      end

      assert_includes(error.message, 'session creation is incomplete')
      assert_empty(tmux.sent_commands)
      refute(File.exist?(portal_log))
    end
  end

  def test_concurrent_attach_cannot_launch_codex_during_initial_delivery
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      goal = File.join(workspace, 'goal.txt')
      portal = File.join(workspace, 'portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(goal, "Deliver the initial request.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        puts JSON.generate(threadId: 'thread-concurrent') if ARGV[1] == 'create'
      RUBY
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      entered_delivery = Queue.new
      release_delivery = Queue.new
      created_session = DevSession::Tmux::Session.new(
        id: '$9', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-concurrent',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2', codex_pane_id: '%1'
      )
      creator_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| created_session }
        define_method(:sync_slug) { |*_arguments, **_keywords| created_session }
        define_method(:revalidate_session!) { |_expected| created_session }
        define_method(:send_portal_goal) do |*_arguments, **_keywords|
          entered_delivery << true
          release_delivery.pop
        end
        define_method(:reconcile_native_client!) do |_slug, session, **_keywords|
          session
        end
      end
      creator = creator_class.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/workspace/tmux.sock',
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [RbConfig.ruby, portal],
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )
      creator_thread = Thread.new do
        creator.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end
      entered_delivery.pop

      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-concurrent',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2',
        pane_current_command: 'sh',
        id: '$9'
      )
      attacher = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/workspace/tmux.sock',
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [RbConfig.ruby, portal],
        tmux:,
        process_exec: ->(*_arguments) { raise 'attach unexpectedly replaced the process' },
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )

      error = assert_raises(DevSession::Error) do
        attacher.attach(slug, as_is: true)
      end
      assert_includes(error.message, 'session creation is incomplete')
      assert_empty(tmux.sent_commands)
    ensure
      release_delivery << true if release_delivery
      creator_thread&.value
    end
  end

  def test_archive_rejects_an_unmerged_registered_branch_before_mutating_state
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "unmerged\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'unmerged feature')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'push', 'origin',
        "refs/heads/#{slug}:refs/heads/#{slug}"
      )
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'feature head is not merged')
      assert_includes(error.message, "#{slug} -> origin/master")
      assert(File.directory?(File.join(workspace, 'work', slug)))
      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_closes_and_commits_a_coordination_only_session
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-coordination'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      File.write(File.join(workspace, 'staged.txt'), "staged\n")
      File.write(File.join(workspace, 'unstaged.txt'), "unstaged\n")
      assert_git_success('git', '-C', workspace, 'add', 'staged.txt')

      runner.archive(slug, as_is: true)

      refute(File.exist?(File.join(workspace, 'work', slug)))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      assert_match(
        /\A---\nlifecycle: complete\n---/,
        File.read(File.join(workspace, 'archive', slug, 'state.md'))
      )
      assert_equal(
        "workspace: archive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
      changed = git_capture_success(
        'git', '-C', workspace, 'show', '--format=', '--name-only', 'HEAD'
      ).lines.map(&:strip).reject(&:empty?)
      refute_includes(changed, 'staged.txt')
      refute_includes(changed, 'unstaged.txt')
      assert_equal('A  staged.txt', git_capture_success(
        'git', '-C', workspace, 'status', '--short', '--', 'staged.txt'
      ).strip)
      assert_equal('?? unstaged.txt', git_capture_success(
        'git', '-C', workspace, 'status', '--short', '--', 'unstaged.txt'
      ).strip)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_retries_a_failed_exact_tracking_commit
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-retry'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, "#!/bin/sh\nexit 1\n")
      File.chmod(0o755, hook)

      assert_raises(DevSession::CommandError) do
        runner.archive(slug, as_is: true)
      end
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal('tracking_archived', journal.fetch('phase'))

      File.unlink(hook)
      runner.archive(slug, as_is: true)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal(
        "workspace: archive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
    end
  end

  def test_archive_retry_does_not_kill_a_same_name_replacement
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-stale-runtime'
      state_home = File.join(workspace, '.xdg-state')
      authority_dir = File.join(workspace, 'authority')
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      interrupted = false
      runner_class = Class.new(DevSession::Runner) do
        define_method(:advance_archive!) do |current_slug, journal, phase|
          super(current_slug, journal, phase)
          if phase == 'thread_retired' && !interrupted
            interrupted = true
            raise DevSession::Error, 'injected pre-runtime interruption'
          end
        end
      end
      first = runner_class.new(
        workspace:, tmux: NullTmux.new, authority_dir:,
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: { 'XDG_STATE_HOME' => state_home }
      )

      assert_raises(DevSession::Error) do
        first.archive(slug, as_is: true)
      end
      journal_path = first.send(:lifecycle_journal_file, slug, 'archive')
      assert_equal('thread_retired', JSON.parse(File.read(journal_path)).fetch('phase'))

      replacement = ReplacedTmux.new(slug, workspace:)
      second = runner_for(
        workspace,
        tmux: replacement,
        authority_dir:,
        env: { 'XDG_STATE_HOME' => state_home }
      )
      stale = DevSession::Tmux::Session.new(
        id: '$11', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      second.send(:write_session_authority, slug, stale, state: 'ready')

      error = assert_raises(DevSession::Error) do
        second.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      refute(replacement.kill_attempted)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
      assert(File.file?(journal_path))
    end
  end

  def test_archive_preflights_legacy_identity_before_creating_a_journal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-legacy-identity'
      authority_dir = File.join(workspace, 'authority')
      tmux = RefusedIdentityInitializationTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(tmux.identity_initialization_attempted)
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_rejects_a_renamed_authority_session_before_creating_a_journal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-renamed-session'
      authority_dir = File.join(workspace, 'authority')
      tmux = RenamedManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      session = DevSession::Tmux::Session.new(
        id: '$11', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, session, state: 'ready')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_resume_upgrades_a_legacy_identity_before_continuing
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-resume-legacy'
      authority_dir = File.join(workspace, 'authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      plan = runner.send(:prepare_cleanup, slug, force: false)
      runner.send(:prepare_archive_journal!, slug, 'complete', {}, plan)

      runner.archive(slug, as_is: true)

      assert(tmux.killed)
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_archive_retry_rejects_mutated_tracking_before_the_commit_phase
    skip 'git is not available' unless command_available?('git')

    %i[state thread].each do |mutation|
      with_workspace do |workspace|
        slug = "2026-06-06-archive-#{mutation}"
        base = runner_for(workspace)
        base.ensure_tracking_files(slug)
        base.send(:ensure_portal_manifest, slug)
        commit_tracking(workspace, slug, lifecycle: 'active')
        configure_workspace_origin(workspace)
        fail_once = true
        runner_class = Class.new(DevSession::Runner) do
          define_method(:advance_archive!) do |current_slug, journal, phase|
            if phase == 'tracking_archived' && fail_once
              fail_once = false
              raise DevSession::Error, 'injected post-move failure'
            end

            super(current_slug, journal, phase)
          end
        end
        runner = runner_class.new(
          workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
          today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
        )

        assert_raises(DevSession::Error) do
          runner.archive(slug, as_is: true)
        end
        assert(File.directory?(File.join(workspace, 'archive', slug)))
        journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
        assert_equal('clusters_released', journal.fetch('phase'))

        if mutation == :state
          state = File.join(workspace, 'archive', slug, 'state.md')
          File.write(state, File.read(state).sub('lifecycle: complete', 'lifecycle: active'))
        else
          portal = File.join(workspace, 'archive', slug, 'portal.yml')
          manifest = YAML.safe_load(File.read(portal))
          manifest['codex']['thread_id'] = 'replacement-thread'
          File.write(portal, YAML.dump(manifest))
        end

        error = assert_raises(DevSession::Error) do
          runner.archive(slug, as_is: true)
        end
        assert_includes(error.message, 'archived tracking changed during recovery')
        assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      end
    end
  end

  def test_unfinished_archive_reserves_the_slug_until_its_matching_retry
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-owned'
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      fail_retirement = true
      runner_class = Class.new(DevSession::Runner) do
        define_method(:retire_portal_thread!) do |*arguments, **options|
          raise DevSession::Error, 'injected retirement failure' if fail_retirement

          super(*arguments, **options)
        end
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal('tracking_committed', journal.fetch('phase'))

      conflicts = [
        -> { runner.delete(slug, as_is: true, force: false) },
        -> { runner.revive(slug, as_is: true) },
        -> do
          runner.start(
            slug, as_is: true, new: false, attach: false, run_codex: false
          )
        end,
        -> do
          runner.worktree_add(
            slug, 'sample', as_is: true, name: nil, branch: nil,
            base: 'master', fetch: false
          )
        end
      ]
      conflicts.each do |operation|
        error = assert_raises(DevSession::Error, &operation)
        assert_includes(error.message, 'session archive is unfinished')
      end

      fail_retirement = false
      runner.archive(slug, as_is: true)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_archive_retry_rejects_dirty_tracking_after_the_commit_phase
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-dirty-commit'
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      runner_class = Class.new(DevSession::Runner) do
        define_method(:retire_portal_thread!) do |*_arguments, **_options|
          raise DevSession::Error, 'injected retirement failure'
        end
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )
      assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal('tracking_committed', journal.fetch('phase'))
      File.open(File.join(workspace, 'archive', slug, 'plan.md'), 'a') do |file|
        file.write("\nUncommitted change.\n")
      end

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'committed archive tracking differs')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_retry_reproves_the_retained_feature_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-reprove'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "merged\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'merged feature')
      commit_tracking(workspace, slug, lifecycle: 'active')
      merge_registered_branches(workspace, slug)
      configure_workspace_origin(workspace)
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, "#!/bin/sh\nexit 1\n")
      File.chmod(0o755, hook)

      assert_raises(DevSession::CommandError) do
        runner.archive(slug, as_is: true)
      end
      File.unlink(hook)
      repository = File.join(workspace, 'repos', 'sample.git')
      temporary = File.join(workspace, 'advanced-feature')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', temporary, slug
      )
      configure_git_identity(temporary)
      File.write(File.join(temporary, 'later.txt'), "not merged\n")
      assert_git_success('git', '-C', temporary, 'add', 'later.txt')
      assert_git_success('git', '-C', temporary, 'commit', '-m', 'later feature')
      assert_git_success('git', '-C', temporary, 'push', 'origin', slug)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'feature branch changed after merge proof')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_archive_reproves_exact_heads_before_each_destructive_retry_phase
    skip 'git is not available' unless command_available?('git')

    %w[quiesced clusters_released].each do |phase|
      with_workspace do |workspace|
        create_bare_repo(workspace, 'sample')
        slug = "2026-06-06-reprove-#{phase.tr('_', '-')}"
        runner = runner_for(workspace)
        runner.worktree_add(
          slug, 'sample', as_is: true, name: nil, branch: nil,
          base: 'master', fetch: false
        )
        path = File.join(workspace, 'worktrees', slug, 'sample')
        configure_git_identity(path)
        File.write(File.join(path, 'feature.txt'), "first merged head\n")
        assert_git_success('git', '-C', path, 'add', 'feature.txt')
        assert_git_success('git', '-C', path, 'commit', '-m', 'first merged feature')
        commit_tracking(workspace, slug, lifecycle: 'active')
        merge_registered_branches(workspace, slug)
        configure_workspace_origin(workspace)

        plan = runner.send(:prepare_cleanup, slug, force: false)
        heads = runner.send(:prove_registered_branches_merged!, slug, plan)
        journal = runner.send(:prepare_archive_journal!, slug, 'complete', heads, plan)
        runner.send(:advance_archive!, slug, journal, 'quiesced')
        if phase == 'clusters_released'
          runner.send(:advance_archive!, slug, journal, 'clusters_released')
        end

        File.write(File.join(path, 'later.txt'), "second merged head\n")
        assert_git_success('git', '-C', path, 'add', 'later.txt')
        assert_git_success('git', '-C', path, 'commit', '-m', 'second merged feature')
        assert_git_success('git', '-C', path, 'push', 'origin', slug)
        assert_git_success('git', '-C', path, 'push', 'origin', "#{slug}:master")

        error = assert_raises(DevSession::Error) do
          runner.archive(slug, as_is: true)
        end
        assert_includes(error.message, 'feature heads changed during archival')
        assert(File.directory?(File.join(workspace, 'work', slug)))
        assert(File.directory?(path))
        persisted = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
        assert_equal(phase, persisted.fetch('phase'))
      end
    end
  end

  def test_archive_accepts_the_exact_feature_head_after_it_is_merged
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-merged'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "merged\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'merged feature')
      commit_tracking(workspace, slug, lifecycle: 'active')
      merge_registered_branches(workspace, slug)
      configure_workspace_origin(workspace)

      runner.archive(slug, as_is: true)

      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'archive', slug, 'portal.yml'))
      )
      repository = manifest.fetch('repositories').fetch(0)
      assert_equal(
        git_capture_success(
          'git', "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
          'rev-parse', "refs/heads/#{slug}"
        ).strip,
        repository.fetch('final_head_sha')
      )
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_archive_abandoned_skips_the_merge_requirement
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-discarded'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'discarded.txt'), "discarded\n")
      assert_git_success('git', '-C', path, 'add', 'discarded.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'discarded')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      runner.archive(slug, as_is: true, abandoned: true)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
      assert_match(
        /\A---\nlifecycle: abandoned\n---/,
        File.read(File.join(workspace, 'archive', slug, 'state.md'))
      )
    end
  end

  def test_revive_commits_tracking_before_starting_the_runtime
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      starts = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:start) do |input, **options|
          starts << [input, options]
        end
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.revive(slug, as_is: true)

      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      assert_equal(1, starts.length)
      assert_equal(true, starts.fetch(0).fetch(1).fetch(:allow_empty_thread))
      assert_equal(false, starts.fetch(0).fetch(1).fetch(:exclusive))
      assert_equal(
        "workspace: revive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_does_not_create_a_blank_thread_for_a_current_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-current-no-thread'
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(base, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      starts = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:start) { |input, **options| starts << [input, options] }
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.revive(slug, as_is: true)

      assert_empty(starts)
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', slug, 'portal.yml'))
      )
      assert_nil(manifest.dig('codex', 'thread_id'))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_resumes_an_interrupted_archive_to_work_transition
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-interrupted'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner_class = Class.new(DevSession::Runner) do
        define_method(:start) { |_input, **_options| nil }
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )
      runner.send(:prepare_revive_journal!, slug, 'complete')
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )

      runner.revive(slug, as_is: true)

      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
      assert_equal(
        "workspace: revive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
    end
  end

  def test_revive_retry_uses_durable_abandoned_confirmation_after_tracking_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-abandoned-retry'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'abandoned')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(
        :prepare_revive_journal!, slug, 'abandoned', abandoned_confirmed: true
      )
      runner.send(:finish_revive_tracking!, slug, journal)

      assert_equal(
        { lifecycle: 'abandoned', pending: true },
        runner.revive_confirmation(slug, as_is: true)
      )
      runner.revive(slug, as_is: true, allow_abandoned: false)

      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_rejects_tracking_edits_after_the_restore_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-tree-change'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner = runner_for(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.write(File.join(workspace, 'work', slug, 'unexpected.txt'), "changed\n")

      error = assert_raises(DevSession::Error) do
        runner.revive(slug, as_is: true)
      end

      assert_includes(error.message, 'revived tracking changed during recovery')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_recovery_rejects_dirty_tracking_after_commit
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-dirty-commit'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner = runner_for(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      runner.send(
        :commit_tracking_transition!, slug, direction: 'revive', mode: 'complete'
      )
      runner.send(:advance_revive!, slug, journal, 'tracking_committed')
      File.open(File.join(workspace, 'work', slug, 'plan.md'), 'a') do |file|
        file.write("\nUncommitted change.\n")
      end

      error = assert_raises(DevSession::Error) do
        runner.revive(slug, as_is: true)
      end

      assert_includes(error.message, 'committed revive tracking differs')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_finalize_rejects_an_unmerged_legacy_worktree_without_a_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-legacy'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.unlink(File.join(workspace, 'work', slug, 'portal.yml'))
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "unmerged legacy feature\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'unmerged legacy feature')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'push', 'origin',
        "refs/heads/#{slug}:refs/heads/#{slug}"
      )
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
      end

      assert_includes(error.message, 'feature head is not merged')
      assert_includes(error.message, "#{slug} -> origin/master")
    end
  end

  def test_archive_refuses_an_active_codex_turn_without_quiescing
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-active-turn'
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
      runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        tmux:,
        portal_command: [RbConfig.ruby, '-e', "warn 'thread is active'; exit 1"],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      runner.send(:write_portal_manifest, slug, manifest)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::CommandError) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'thread is active')
      refute(tmux.quiesced)
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_finalize_accepts_an_unmerged_abandoned_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'discarded.txt'), "discarded feature\n")
      assert_git_success('git', '-C', path, 'add', 'discarded.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'discarded feature')
      commit_tracking(workspace, slug, lifecycle: 'abandoned')

      finalize_core(runner, slug, as_is: true)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_revive_current_archive_clears_terminal_metadata
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      merge_registered_branches(workspace, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))

      state = File.read(File.join(workspace, 'work', slug, 'state.md'))
      assert_match(/\A---\nlifecycle: active\n---\n/, state)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(manifest.key?('finalized_at'))
      refute(manifest.dig('repositories', 0).key?('final_head_sha'))
      assert_equal('revived', manifest.dig('creation', 'tracking_origin'))
      refute(File.exist?(File.join(workspace, 'archive', slug)))

      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: nil, fetch: false
      )
      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
    end
  end

  def test_revive_journal_repeats_parent_sync_after_an_interrupted_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-interrupted'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runtime = File.join(workspace, 'runtime-authority')
      runner = runner_for(workspace, authority_dir: runtime)
      runner.send(:prepare_revive_journal!, slug, 'complete')
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )

      FileUtils.rm_rf(runtime)
      synced = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:fsync_directory) do |path|
          synced << path
          super(path)
        end
      end
      retry_runner = runner_class.new(
        workspace:,
        authority_dir: runtime,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      journal = retry_runner.send(:load_revive_journal, slug)
      retry_runner.send(:finish_revive_tracking!, slug, journal)

      state = File.read(File.join(workspace, 'work', slug, 'state.md'))
      assert_match(/\A---\nlifecycle: active\n---\n/, state)
      refute(File.exist?(File.join(workspace, 'archive', slug)))
      assert(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.revive.json")))
      assert_includes(synced, File.join(workspace, 'archive'))
      assert_includes(synced, File.join(workspace, 'work'))
    end
  end

  def test_revive_journal_rejects_a_changed_plan_after_an_interrupted_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-interrupted-plan'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner = runner_for(workspace)
      runner.send(:prepare_revive_journal!, slug, 'complete')
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )
      File.write(File.join(workspace, 'work', slug, 'plan.md'), "truncated\n")

      error = assert_raises(DevSession::Error) do
        journal = runner.send(:load_revive_journal, slug)
        runner.send(:finish_revive_tracking!, slug, journal)
      end
      assert_match(/revived plan changed after revive was prepared/, error.message)
      assert(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.revive.json")))
    end
  end

  def test_revive_with_an_existing_thread_records_exact_recovery_provenance
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-existing-thread'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex']['thread_id'] = 'thread-existing'
      manifest['creation']['initial_goal_sent'] = true
      runner.send(:write_portal_manifest, slug, manifest)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)

      revived = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-existing', revived.dig('codex', 'thread_id'))
      assert_equal('revived', revived.dig('creation', 'tracking_origin'))
      assert_match(/\A[0-9a-f]{64}\z/, revived.dig('creation', 'tracking_plan_sha256'))
      assert_match(/\A[0-9a-f]{64}\z/, revived.dig('creation', 'tracking_state_sha256'))
    end
  end

  def test_start_revived_existing_thread_uses_exact_archived_recovery_without_a_goal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-existing-thread-recovery'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {
        'thread_id' => 'thread-existing',
        'socket_path' => '/run/current/app-server.sock',
        'client_version' => '0.152.1'
      }
      manifest['creation']['initial_goal_sent'] = true
      runner.send(:write_portal_manifest, slug, manifest)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))

      calls = File.join(workspace, 'portal-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{calls.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        puts JSON.generate(threadId: 'thread-existing') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$revived', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-existing',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.152.1', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) { |*_args, **_options| session }
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      starter = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal],
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        json: true,
        exclusive: false
      )

      assert_equal('thread-existing', JSON.parse(out.string).fetch('threadId'))
      recorded = File.read(calls)
      assert_includes(recorded, 'thread create')
      assert_includes(recorded, '--thread-id thread-existing')
      assert_includes(recorded, '--recover-archived')
      refute_includes(recorded, 'ensure-initial')
      updated = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-existing', updated.dig('codex', 'thread_id'))
      refute(updated.dig('creation').key?('tracking_origin'))
      refute(updated.dig('creation').key?('tracking_plan_sha256'))
      refute(updated.dig('creation').key?('tracking_state_sha256'))
    end
  end

  def test_revived_thread_recovery_defers_version_refresh_until_authority_is_ready
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-existing-thread-retry'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {
        'thread_id' => 'thread-existing',
        'socket_path' => '/run/current/app-server.sock',
        'client_version' => '0.152.1'
      }
      manifest['creation']['initial_goal_sent'] = true
      runner.send(:write_portal_manifest, slug, manifest)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))

      calls = File.join(workspace, 'portal-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{calls.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        puts JSON.generate(threadId: 'thread-existing') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$revived', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-existing',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.153.4', codex_pane_id: '%1'
      )
      authority_attempts = 0
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:write_session_authority) do |*_args, **_options|
          authority_attempts += 1
          if authority_attempts == 1
            raise DevSession::Error, 'simulated authority publication failure'
          end
        end
        define_method(:sync_slug) { |*_args, **_options| session }
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      starter = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.153.4',
        portal_command: [RbConfig.ruby, portal],
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(DevSession::Error) do
        starter.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          json: true,
          exclusive: false
        )
      end
      assert_match(/simulated authority publication failure/, error.message)
      interrupted = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('0.152.1', interrupted.dig('codex', 'client_version'))
      assert_equal('revived', interrupted.dig('creation', 'tracking_origin'))

      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        json: true,
        exclusive: false
      )

      assert_equal('thread-existing', JSON.parse(out.string).fetch('threadId'))
      assert_equal(2, authority_attempts)
      assert_equal(2, File.readlines(calls).count { |line| line.start_with?('thread create ') })
      recovered = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('0.153.4', recovered.dig('codex', 'client_version'))
      refute(recovered.dig('creation').key?('tracking_origin'))
      refute(recovered.dig('creation').key?('tracking_plan_sha256'))
      refute(recovered.dig('creation').key?('tracking_state_sha256'))
    end
  end

  def test_revive_legacy_archive_reuses_retained_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-demo'
      assert_git_success('git', "--git-dir=#{repository}", 'branch', slug, 'master')
      master = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success(
        'git', "--git-dir=#{repository}", 'update-ref',
        'refs/remotes/origin/master', master
      )
      assert_git_success(
        'git', "--git-dir=#{repository}", 'symbolic-ref',
        'refs/remotes/origin/HEAD', 'refs/remotes/origin/master'
      )
      tracking = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(tracking)
      runner = runner_for(workspace)
      File.write(
        File.join(tracking, 'plan.md'),
        <<~PLAN
          # Retained legacy plan

          This substantive plan predates the current tracking template.
        PLAN
      )
      File.write(
        File.join(tracking, 'state.md'),
        <<~STATE
          ---
          lifecycle: active
          ---

          # Retained legacy state

          This substantive state predates the current tracking template.
        STATE
      )
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))
      revived = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('revived', revived.dig('creation', 'tracking_origin'))
      assert_equal(
        Digest::SHA256.hexdigest(File.read(File.join(workspace, 'work', slug, 'plan.md'))),
        revived.dig('creation', 'tracking_plan_sha256')
      )
      assert_equal(
        Digest::SHA256.hexdigest(File.read(File.join(workspace, 'work', slug, 'state.md'))),
        revived.dig('creation', 'tracking_state_sha256')
      )
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: nil, fetch: false
      )

      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_equal(slug, git_capture_success('git', '-C', path, 'branch', '--show-current').strip)
      assert_equal(master, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal(master, manifest.dig('repositories', 0, 'initial_base_sha'))

      plan_before = File.read(File.join(workspace, 'work', slug, 'plan.md'))
      state_before = File.read(File.join(workspace, 'work', slug, 'state.md'))
      goal = File.join(workspace, 'goal.txt')
      portal = File.join(workspace, 'portal.rb')
      File.write(goal, "Continue the retained initiative.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        puts JSON.generate(threadId: 'thread-fresh') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$fresh', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-fresh',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.152.1', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) { |*_args, **_options| session }
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      starter = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal],
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )

      assert_equal('thread-fresh', JSON.parse(out.string).fetch('threadId'))
      assert_equal(plan_before, File.read(File.join(workspace, 'work', slug, 'plan.md')))
      assert_equal(state_before, File.read(File.join(workspace, 'work', slug, 'state.md')))
      updated = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal(master, updated.dig('repositories', 0, 'initial_base_sha'))
      assert_equal('thread-fresh', updated.dig('codex', 'thread_id'))
      assert_equal('ready', updated.dig('creation', 'state'))
      refute(updated.dig('creation').key?('tracking_origin'))
      refute(updated.dig('creation').key?('tracking_plan_sha256'))
      refute(updated.dig('creation').key?('tracking_state_sha256'))

      out.truncate(0)
      out.rewind
      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )
      assert_equal('thread-fresh', JSON.parse(out.string).fetch('threadId'))

      journal_path = starter.send(:creation_journal_file, slug)
      interrupted = JSON.parse(File.read(journal_path)).merge('state' => 'creating')
      interrupted['tmux_identity'] = 'a' * 64
      File.write(journal_path, JSON.generate(interrupted))
      File.write(
        File.join(workspace, 'work', slug, 'plan.md'),
        "#{plan_before}\nFollow-up recorded after the initial turn.\n"
      )
      out.truncate(0)
      out.rewind
      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: nil,
        json: true,
        exclusive: false
      )
      assert_equal('thread-fresh', JSON.parse(out.string).fetch('threadId'))
      completed_journal = JSON.parse(File.read(journal_path))
      assert_equal('ready', completed_journal.fetch('state'))
      refute(completed_journal.key?('tmux_identity'))
    end
  end

  def test_start_adopts_committed_active_tracking_and_registers_existing_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-retained'
      master = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success('git', "--git-dir=#{repository}", 'branch', slug, 'master')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'update-ref',
        'refs/remotes/origin/master', master
      )
      assert_git_success(
        'git', "--git-dir=#{repository}", 'symbolic-ref',
        'refs/remotes/origin/HEAD', 'refs/remotes/origin/master'
      )
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      FileUtils.mkdir_p(File.dirname(worktree))
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', worktree, slug
      )

      tracking = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(tracking)
      plan = "# Retained plan\n\nPokračovat v existující implementaci.\n"
      state = "---\nlifecycle: active\n---\n\n# Retained state\n\nPřipraveno.\n" + ('x' * 1_100_000)
      File.write(File.join(tracking, 'plan.md'), plan)
      File.write(File.join(tracking, 'state.md'), state)
      commit_tracking(workspace, slug, lifecycle: 'active')

      goal = File.join(workspace, 'goal.txt')
      portal = File.join(workspace, 'portal.rb')
      File.write(goal, "Read plan.md and state.md, then continue the retained initiative.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        puts JSON.generate(threadId: 'thread-retained') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$retained', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-retained',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.152.1', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) do |selected_slug, **_options|
          sync_portal_repositories(selected_slug, worktree_entries(selected_slug))
          session
        end
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal],
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      interrupted = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      assert_equal('retained', interrupted.fetch('tracking_origin'))
      refute(File.exist?(File.join(tracking, 'portal.yml')))

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

      assert_equal('thread-retained', JSON.parse(out.string).fetch('threadId'))
      assert_equal(plan.b, File.binread(File.join(tracking, 'plan.md')))
      assert_equal(state.b, File.binread(File.join(tracking, 'state.md')))
      manifest = YAML.safe_load(File.read(File.join(tracking, 'portal.yml')))
      assert_equal('thread-retained', manifest.dig('codex', 'thread_id'))
      assert_equal('sample', manifest.dig('repositories', 0, 'project'))
      assert_equal(slug, manifest.dig('repositories', 0, 'branch'))
      assert_equal('master', manifest.dig('repositories', 0, 'default_branch'))
      assert_equal(master, manifest.dig('repositories', 0, 'initial_base_sha'))
      refute(manifest.fetch('creation').key?('tracking_origin'))

      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal('ready', journal.fetch('state'))
      assert_equal('retained', journal.fetch('tracking_origin'))
      assert_equal(Digest::SHA256.hexdigest(plan), journal.fetch('tracking_plan_sha256'))
      assert_equal(Digest::SHA256.hexdigest(state), journal.fetch('tracking_state_sha256'))
      refute(journal.key?('tracking_portal_sha256'))
    end
  end

  def test_start_refuses_uncommitted_retained_plan_or_state
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-dirty'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      File.write(File.join(workspace, 'work', slug, 'plan.md'), "uncommitted replacement\n")
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

      error = assert_raises(DevSession::Error) do
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
      assert_match(/retained plan, state, and portal absence must match/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
    end
  end

  def test_start_refuses_a_deleted_committed_retained_portal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-deleted-portal'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      File.unlink(File.join(workspace, 'work', slug, 'portal.yml'))
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

      error = assert_raises(DevSession::Error) do
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
      assert_match(/retained plan, state, and portal absence must match/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
    end
  end

  def test_start_refuses_retained_tracking_without_codex_before_mutation
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-no-codex'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

      error = assert_raises(DevSession::Error) do
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
      assert_match(/restarting retained tracking requires a Codex conversation/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
    end
  end

  def test_start_without_goal_refuses_retained_tracking_without_codex_before_mutation
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-no-goal-no-codex'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          json: true,
          exclusive: true
        )
      end
      assert_match(/restarting retained tracking requires a Codex conversation/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(runner.instance_variable_get(:@tmux).session(slug))
    end
  end

  def test_start_without_goal_refuses_deleted_committed_portal_without_codex_before_mutation
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-deleted-portal-no-codex'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      File.unlink(File.join(workspace, 'work', slug, 'portal.yml'))

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          json: true,
          exclusive: true
        )
      end
      assert_match(/restarting retained tracking requires a Codex conversation/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(runner.instance_variable_get(:@tmux).session(slug))
    end
  end

  def test_start_without_goal_allows_a_stopped_ready_session_without_codex
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-ready-no-codex'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(slug)
      setup.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      session = DevSession::Tmux::Session.new(
        id: '$restarted', name: slug, mark: '1', slug:, workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |expected| expected }
      end
      out = StringIO.new
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: false,
        json: true,
        exclusive: false
      )

      result = JSON.parse(out.string)
      assert_equal(slug, result.fetch('slug'))
      assert_nil(result.fetch('threadId'))
      assert_equal(['tmux', 'attach-session', '-t', '$restarted:'], result.fetch('attach'))
    end
  end

  def test_start_refuses_committed_empty_portal_as_retained_tracking
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-empty-portal'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

      error = assert_raises(DevSession::Error) do
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
      assert_match(/retained active tracking with a portal manifest cannot be adopted/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
    end
  end

  def test_start_refuses_manifestless_tracking_with_archive_history
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-manually-restored-archive'
      runner = archived_runner(workspace, slug)
      FileUtils.mv(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )
      FileUtils.rm_f(File.join(workspace, 'work', slug, 'portal.yml'))
      set_lifecycle(workspace, slug, 'active')
      assert_git_success(
        'git', '-C', workspace, 'add', '-A', '--',
        File.join('work', slug), File.join('archive', slug)
      )
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'manually restore archive')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

      error = assert_raises(DevSession::Error) do
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
      assert_match(/archived slug must be restored with dev-session revive/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
    end
  end

  def test_retained_conversation_replay_rejects_changed_manifest_provenance
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-manifest-replay'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")
      journal = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: journal)
      manifest['creation']['tracking_state_sha256'] = '0' * 64
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::Error) do
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
      assert_match(/preserved tracking provenance changed during creation/, error.message)
    end
  end

  def test_portal_sync_leaves_an_unproven_unregistered_worktree_untouched
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-unproven'
      path = File.join(workspace, 'worktrees', slug, 'legacy-clone')
      assert_git_success('git', 'init', '-b', slug, path)
      marker = File.join(path, 'uncommitted.txt')
      File.write(marker, "keep this work\n")
      plain_path = File.join(workspace, 'worktrees', slug, 'plain-directory')
      FileUtils.mkdir_p(plain_path)
      plain_marker = File.join(plain_path, 'keep.txt')
      File.write(plain_marker, "keep this too\n")
      err = StringIO.new
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err:, today: TODAY
      )
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)

      runner.send(
        :sync_portal_repositories,
        slug,
        runner.send(:worktree_entries, slug)
      )

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_empty(manifest.fetch('repositories'))
      assert_equal("keep this work\n", File.read(marker))
      assert_equal("keep this too\n", File.read(plain_marker))
      assert_match(/did not register unproven worktree/, err.string)
      assert_match(/outside the canonical repository root/, err.string)
      assert_match(/not a canonical attached Git worktree/, err.string)
    end
  end

  def test_portal_sync_leaves_a_canonical_worktree_with_an_unsafe_name_unregistered
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-unsafe-worktree-name'
      master = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success('git', "--git-dir=#{repository}", 'branch', slug, 'master')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'update-ref',
        'refs/remotes/origin/master', master
      )
      assert_git_success(
        'git', "--git-dir=#{repository}", 'symbolic-ref',
        'refs/remotes/origin/HEAD', 'refs/remotes/origin/master'
      )
      path = File.join(workspace, 'worktrees', slug, 'legacy checkout')
      FileUtils.mkdir_p(File.dirname(path))
      assert_git_success('git', "--git-dir=#{repository}", 'worktree', 'add', path, slug)
      err = StringIO.new
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err:, today: TODAY
      )
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)

      proven = runner.send(
        :sync_portal_repositories,
        slug,
        runner.send(:worktree_entries, slug)
      )

      assert_empty(proven)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_empty(manifest.fetch('repositories'))
      assert_match(/worktree name is unsafe for a portal manifest/, err.string)
      assert(File.directory?(path))
    end
  end

  def test_portal_sync_rejects_a_registered_non_worktree_path
    with_workspace do |workspace|
      slug = '2026-06-06-registered-non-worktree'
      path = File.join(workspace, 'worktrees', slug, 'broken')
      FileUtils.mkdir_p(path)
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['repositories'] = [{
        'name' => 'broken',
        'project' => 'sample',
        'branch' => slug,
        'default_branch' => 'master'
      }]
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::Error) do
        runner.send(:sync_portal_repositories, slug, [])
      end
      assert_match(/registered portal worktree is not a canonical attached worktree/, error.message)
      assert(File.directory?(path))
    end
  end

  def test_sync_does_not_open_a_window_for_an_unproven_git_directory
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-unproven-window'
      path = File.join(workspace, 'worktrees', slug, 'standalone')
      assert_git_success('git', 'init', '-b', slug, path)
      tmux = WindowRecordingTmux.new(slug, workspace:)
      err = StringIO.new
      runner = DevSession::Runner.new(
        workspace:, tmux:, out: StringIO.new, err:, today: TODAY, env: {}
      )
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)

      runner.send(:sync_slug, slug, require_session: true)

      assert_empty(tmux.captures)
      assert_match(/did not register unproven worktree/, err.string)
      assert(File.directory?(path))
    end
  end

  def test_fresh_conversation_rejects_terminal_revived_tracking
    %w[complete abandoned].each do |lifecycle|
      with_workspace do |workspace|
        slug = "2026-06-06-#{lifecycle}"
        tracking = File.join(workspace, 'work', slug)
        FileUtils.mkdir_p(tracking)
        FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug))
        plan = "# Retained plan\n\nContinue this work.\n"
        state = "---\nlifecycle: #{lifecycle}\n---\n\n# Retained state\n"
        File.write(File.join(tracking, 'plan.md'), plan)
        File.write(File.join(tracking, 'state.md'), state)
        runner = runner_for(workspace)
        manifest = runner.send(
          :revived_portal_manifest,
          slug,
          plan_sha256: Digest::SHA256.hexdigest(plan),
          state_sha256: Digest::SHA256.hexdigest(state)
        )
        File.write(File.join(tracking, 'portal.yml'), YAML.dump(manifest))
        goal = File.join(workspace, 'goal.txt')
        File.write(goal, "Resume retained work.\n")

        error = assert_raises(DevSession::Error) do
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
        assert_match(/cannot create a conversation for a #{lifecycle} initiative/, error.message)
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
      end
    end
  end

  def test_revived_conversation_replay_rejects_changed_tracking
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revived-replay'
      runner = archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume retained work.\n")
      journal = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      assert(journal.fetch('preserve_tracking'))
      File.write(File.join(workspace, 'work', slug, 'plan.md'), "x")

      error = assert_raises(DevSession::Error) do
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
      assert_match(/preserved tracking changed before conversation creation/, error.message)
    end
  end

  def test_revived_conversation_rejects_self_asserted_uncommitted_provenance
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-self-asserted-revive'
      runner = archived_runner(workspace, slug)
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug))
      plan = "# Replacement plan\n\nThis did not come from the archive.\n"
      state_path = File.join(workspace, 'work', slug, 'state.md')
      state = runner.send(:revived_state_content, File.read(state_path))
      File.write(File.join(workspace, 'work', slug, 'plan.md'), plan)
      File.write(state_path, state)
      manifest = runner.send(
        :revived_portal_manifest,
        slug,
        plan_sha256: Digest::SHA256.hexdigest(plan),
        state_sha256: Digest::SHA256.hexdigest(state)
      )
      File.write(File.join(workspace, 'work', slug, 'portal.yml'), YAML.dump(manifest))
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume retained work.\n")

      error = assert_raises(DevSession::Error) do
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
      assert_match(/revived tracking provenance cannot be proven/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
    end
  end

  def test_revive_refuses_abandoned_dirty_duplicate_and_live_states
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-abandoned'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'abandoned')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      error = assert_raises(DevSession::Error) { runner.revive(slug, as_is: true) }
      assert_includes(error.message, 'without confirmation')
      runner.send(:prepare_revive_journal!, slug, 'abandoned', abandoned_confirmed: true)
      runner.send(:finish_revive_tracking!, slug, runner.send(:load_revive_journal, slug))
      assert_match(/lifecycle: active/, File.read(File.join(workspace, 'work', slug, 'state.md')))
    end

    with_workspace do |workspace|
      slug = '2026-06-06-dirty'
      runner = archived_runner(workspace, slug)
      File.write(File.join(workspace, 'archive', slug, 'state.md'), "\nchanged\n", mode: 'a')
      error = assert_raises(DevSession::Error) do
        runner.send(:prepare_revive_journal!, slug, 'complete')
      end
      assert_includes(error.message, 'archive move must be committed')
    end

    with_workspace do |workspace|
      slug = '2026-06-06-duplicate'
      runner = archived_runner(workspace, slug)
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      error = assert_raises(DevSession::Error) do
        runner.send(:prepare_revive_journal!, slug, 'complete')
      end
      assert_includes(error.message, 'active tracking already exists')
    end

    with_workspace do |workspace|
      slug = '2026-06-06-live'
      archived_runner(workspace, slug)
      tmux = ManagedTmux.new(slug, workspace:)
      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).send(:prepare_revive_journal!, slug, 'complete')
      end
      assert_includes(error.message, 'live tmux session')
    end
  end

  private

  def removal_recovery(workspace, slug)
    matches = Dir.glob(
      File.join(workspace, '.xdg-state', 'dev-workspaces', 'removed', '*', "*-#{slug}-*")
    )
    assert_equal(1, matches.length, "expected one recovery directory for #{slug}")
    matches.fetch(0)
  end

  def with_workspace
    Dir.mktmpdir('dev-session-test') do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'repos'))
      FileUtils.mkdir_p(File.join(workspace, 'work'))
      FileUtils.mkdir_p(File.join(workspace, 'worktrees'))
      yield workspace
    end
  end

  def profile_link_token(path)
    DevWorkspaceProfileIdentity.token(path)
  end

  def cleanup_contract_helper(workspace, name, paths)
    helper = File.join(workspace, "#{name}-cleanup-contract")
    payload = JSON.generate('schema' => 1, 'paths' => paths)
    File.write(helper, <<~SH)
      #!/bin/sh
      [ "$1" = cleanup-paths ] || exit 0
      printf '%s\n' #{Shellwords.escape(payload)}
    SH
    File.chmod(0o755, helper)
    helper
  end

  def runner_for(
    workspace,
    env: {},
    cwd: nil,
    tmux: nil,
    out: nil,
    authority_dir: nil,
    alpha_cluster: nil,
    beta_cluster: nil
  )
    out ||= StringIO.new
    tmux ||= NullTmux.new
    resolved_env = {
      'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')
    }.merge(env)
    cluster_providers = { }
    cluster_providers['alpha'] = alpha_cluster if alpha_cluster
    cluster_providers['beta'] = beta_cluster if beta_cluster

    DevSession::Runner.new(
      workspace:,
      authority_dir:,
      tmux:,
      out:,
      err: StringIO.new,
      today: TODAY,
      env: resolved_env,
      cwd: cwd || workspace,
      cluster_providers:
    )
  end

  def test_lifecycle_phase_output_uses_the_persisted_phase_name
    Dir.mktmpdir do |workspace|
      out = StringIO.new
      runner = runner_for(workspace, out:)

      runner.send(:announce_lifecycle_phase, 'archive', 'clusters_released')

      assert_equal("archive: clusters released\n", out.string)
    end
  end

  def archived_runner(workspace, slug)
    runner = runner_for(workspace)
    runner.ensure_tracking_files(slug)
    commit_tracking(workspace, slug, lifecycle: 'complete')
    finalize_core(runner, slug, as_is: true)
    commit_archive_move(workspace, slug)
    runner
  end

  def finalize_core(runner, input, as_is:)
    slug = runner.send(:lookup_slug, input, as_is:)
    runner.send(:select_tmux_for_slug!, slug)
    runner.send(:finalize_locked!, slug)
  end

  def create_bare_repo(workspace, project)
    source = File.join(workspace, "source-#{project}")
    bare = File.join(workspace, 'repos', "#{project}.git")

    assert_git_success('git', 'init', '-b', 'master', source)
    assert_git_success('git', '-C', source, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', source, 'config', 'user.name', 'Test User')
    assert_git_success('git', '-C', source, 'config', 'receive.denyCurrentBranch', 'updateInstead')
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

  def configure_workspace_origin(workspace)
    remote = File.join(workspace, '.git', 'test-origin.git')
    assert_git_success('git', 'init', '--bare', remote)
    assert_git_success('git', '-C', workspace, 'remote', 'add', 'origin', remote)
    assert_git_success('git', '-C', workspace, 'push', '-u', 'origin', 'master')
  end

  def merge_registered_branches(workspace, slug)
    path = File.join(workspace, 'work', slug, 'portal.yml')
    return unless File.file?(path)

    manifest = YAML.safe_load(File.read(path))
    manifest.fetch('repositories', []).each do |repository|
      common = if repository.fetch('project') == 'workspace'
                 File.join(workspace, '.git')
               else
                 File.join(workspace, 'repos', "#{repository.fetch('project')}.git")
               end
      branch = repository.fetch('branch')
      default = repository.fetch('default_branch')
      assert_git_success(
        'git', "--git-dir=#{common}", 'push', 'origin',
        "refs/heads/#{branch}:refs/heads/#{branch}"
      )
      assert_git_success(
        'git', "--git-dir=#{common}", 'push', 'origin',
        "refs/heads/#{branch}:refs/heads/#{default}"
      )
    end
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
    target = DevSession::Tmux.session_target(slug)
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
    return false if ENV['DEV_SESSION_SKIP_REAL_TMUX_TESTS'] == '1'
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
