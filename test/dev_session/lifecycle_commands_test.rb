# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
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

    def with_timeout(seconds, &block)
      @delegate.with_timeout(seconds, &block)
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

end
