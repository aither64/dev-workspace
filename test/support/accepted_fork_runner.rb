# frozen_string_literal: true

# Subprocess adapter for creation_source_lock_test.go. The ordinary CLI, source
# checks, flock helpers, request binding, journals and completion writers remain
# real. Only native/provider/tmux effects are replaced with counted fixtures.
require 'json'
require 'socket'

if ARGV.first == '--hold'
  ARGV.shift
  path, device, inode = ARGV
  File.open(path, File::RDWR | File::NOFOLLOW) do |file|
    stat = file.stat
    abort 'holder lock identity changed' unless stat.dev == Integer(device) && stat.ino == Integer(inode)
    abort 'holder could not acquire SH' unless file.flock(File::LOCK_SH | File::LOCK_NB)
    emit = lambda do |kind|
      puts JSON.generate(kind:, pid: Process.pid, mode: 'SH', device: stat.dev, inode: stat.ino,
                         monotonic: Process.clock_gettime(Process::CLOCK_MONOTONIC))
      $stdout.flush
    end
    emit.call('holder_acquired')
    abort 'holder release handshake timed out' unless IO.select([$stdin], nil, nil, 5)
    abort 'invalid holder release handshake' unless $stdin.gets == "release\n"
    file.flock(File::LOCK_UN)
    emit.call('holder_released')
  ensure
    file.flock(File::LOCK_UN)
  end
  exit 0
end

ACCEPTED_FORK_CONFIG = JSON.parse(File.read(ARGV.shift))
load File.expand_path('../../libexec/dev-session', __dir__)

module AcceptedForkFixture
  def self.event(kind, **fields)
    event = { kind:, pid: Process.pid, monotonic: Process.clock_gettime(Process::CLOCK_MONOTONIC) }.merge(fields)
    File.open(ACCEPTED_FORK_CONFIG.fetch('events'), 'a', 0o600) do |file|
      file.puts JSON.generate(event)
      file.flush
      file.fsync
    end
    return unless kind == 'source_attempt' && fields.fetch(:stage) == ACCEPTED_FORK_CONFIG.fetch('pauseStage')

    UNIXSocket.open(ACCEPTED_FORK_CONFIG.fetch('socket')) do |socket|
      socket.puts JSON.generate(event)
      socket.flush
      raise DevSession::Error, 'source stage handshake timed out' unless IO.select([socket], nil, nil, 5)
      raise DevSession::Error, 'invalid source stage handshake' unless socket.gets == "continue\n"
    end
  end

  class ExternalCommands
    class Success
      def success?
        true
      end
    end

    def option(argv, name)
      argv.fetch(argv.index(name) + 1)
    end

    def capture(argv, **_options)
      output = ''
      if argv[0, 3] == ['fixture-portal', 'thread', 'resolve-fork-settings']
        source = option(argv, '--thread-id')
        model, effort, provider = %w[--model --effort --provider].map { |key| option(argv, key) }
        AcceptedForkFixture.event('resolve_settings', sourceThread: source, model:, effort:, provider:)
        raise DevSession::Error, 'unexpected fixture settings' unless source == 'thread-1' &&
          model == 'model-1' && effort == 'high' && provider == 'openai'
        output = JSON.generate('model' => model, 'reasoningEffort' => effort)
      elsif argv[0, 3] == ['fixture-portal', 'thread', 'fork']
        AcceptedForkFixture.event('native_fork', slug: option(argv, '--session-slug'),
          sourceThread: option(argv, '--thread-id'), fresh: argv.include?('--fresh'),
          model: option(argv, '--model'), effort: option(argv, '--effort'))
        output = JSON.generate('threadId' => 'thread-fork')
      elsif argv[0, 3] == ['fixture-portal', 'thread', 'require-materialized']
        AcceptedForkFixture.event('native_materialization', threadId: option(argv, '--thread-id'))
      elsif argv[0, 3] == ['fixture-portal', 'thread', 'set-name']
        AcceptedForkFixture.event('native_name', slug: option(argv, '--name'), threadId: option(argv, '--thread-id'))
      elsif argv[0, 3] == ['fixture-portal', 'thread', 'require-idle']
        AcceptedForkFixture.event('native_idle_observation')
      elsif argv[0, 3] == ['fixture-portal', 'thread', 'ensure-settings']
        thread = option(argv, '--thread-id')
        model, effort = %w[--model --effort].map { |key| option(argv, key) }
        AcceptedForkFixture.event('native_settings', threadId: thread, model:, effort:)
        raise DevSession::Error, 'unexpected recovery settings' unless thread == 'thread-fork' &&
          model == 'model-1' && effort == 'high'
        output = JSON.generate('model' => model, 'reasoningEffort' => effort)
      elsif argv[0, 3] == [File.join(ACCEPTED_FORK_CONFIG.fetch('hostProfile'), 'bin/workspace-host'), 'provider', 'bind-creation']
        name, slug, identity, provider = argv.drop(3)
        AcceptedForkFixture.event('provider_bind', slug:, provider:, creationIdentity: identity)
        raise DevSession::Error, 'unexpected fixture provider' unless name == 'test-workspace' && provider == 'openai'
      elsif argv == [ACCEPTED_FORK_CONFIG.fetch('nativeExecutable'), '--version']
        output = 'codex ' + ACCEPTED_FORK_CONFIG.fetch('version')
      else
        kind = { 'create' => 'native_start', 'ensure-initial' => 'native_send' }[argv[2]]
        AcceptedForkFixture.event(kind) if kind
        raise DevSession::Error, 'unexpected external command in accepted-fork fixture'
      end
      [output, '', Success.new]
    end

    def run(*)
      raise DevSession::Error, 'unexpected external command in accepted-fork fixture'
    end
  end

  class Tmux
    attr_accessor :created, :command

    def session(slug)
      created if created&.slug == slug
    end

    def session_by_id(id)
      created if created&.id == id
    end

    def managed_session_identities
      [created].compact
    end

    def windows(_id)
      []
    end

    def run(*args)
      # Simulate only the external terminal process; Runner validates ownership.
      self.command = File.basename(ACCEPTED_FORK_CONFIG.fetch('shell')) if args.first == 'respawn-pane'
      self.command = 'codex' if args.first == 'send-keys' && args.last == 'Enter'
    end

    def pane_session_id(pane)
      created.id if pane == created&.codex_pane_id
    end

    def pane_current_command(_pane)
      command
    end

    def argv(*args)
      ['fixture-tmux', *args]
    end
  end

  module RunnerEffects
    def initialize(**options)
      # Never discover the installed portal, provider state, or live tmux.
      safe_env = {
        'PATH' => '', 'SHELL' => ACCEPTED_FORK_CONFIG.fetch('shell'),
        'DEV_WORKSPACES_STATE' => ACCEPTED_FORK_CONFIG.fetch('state'),
        'DEV_WORKSPACE_NAME' => 'test-workspace',
        'DEV_WORKSPACES_CODEX_PROVIDERS' => ACCEPTED_FORK_CONFIG.fetch('absentCatalog'),
        DevSession::CREATION_PROGRESS_ENV => ENV.fetch(DevSession::CREATION_PROGRESS_ENV, '0')
      }
      super(**options.merge(tmux: Tmux.new, command_runner: ExternalCommands.new,
                            portal_command: ['fixture-portal'], codex_command: ACCEPTED_FORK_CONFIG.fetch('nativeExecutable'),
                            require_runtime: false, env: safe_env))
      @fixture_source_acquisitions = 0
    end

    def with_slug_lock(slug, &block)
      return super unless slug == ACCEPTED_FORK_CONFIG.fetch('source')

      @fixture_source_acquisitions += 1
      stage = @fixture_source_acquisitions
      stat = File.stat(session_lock_file(slug))
      details = { stage:, mode: 'EX', device: stat.dev, inode: stat.ino }
      AcceptedForkFixture.event('source_attempt', **details)
      super(slug) do
        AcceptedForkFixture.event('source_acquired', **details)
        block.call
      end
    rescue DevSession::SessionBusyError
      raise unless slug == ACCEPTED_FORK_CONFIG.fetch('source')
      AcceptedForkFixture.event('source_refused', **details)
      raise
    end

    def create_tmux_session(slug, run_codex:, thread_id:, identity_token:, model:, effort:, launch_codex: run_codex)
      AcceptedForkFixture.event('tmux_attempt', slug:)
      raise DevSession::Error, 'fixture terminal interruption' if ACCEPTED_FORK_CONFIG.fetch('failTerminal')
      raise DevSession::Error, 'fixture terminal must run Codex' unless run_codex
      AcceptedForkFixture.event('tmux_create', slug:, threadId: thread_id, model:, effort:)
      @tmux.command = launch_codex ? 'codex' : File.basename(@shell_command)
      @tmux.created = DevSession::Tmux::Session.new(
        id: '$42', name: slug, mark: '1', slug:, environment_slug: slug, workspace:,
        socket_path: ACCEPTED_FORK_CONFIG.fetch('tmuxSocket'), codex_thread_id: thread_id,
        codex_socket_path: @codex_socket, codex_client_version: @codex_version, codex_pane_id: '%42', identity_token:
      )
    end
  end
end

DevSession::Runner.prepend(AcceptedForkFixture::RunnerEffects)
AcceptedForkFixture.event('cli_started')
# Supply the disposable profile identity as the installed executable wrapper
# does. CLI generation validation itself is not replaced.
profile = ACCEPTED_FORK_CONFIG.fetch('hostProfile')
fixed = ['--host-profile', profile, '--expected-host-generation', File.realpath(profile),
         '--expected-host-profile-token', DevWorkspaceProfileIdentity.token(profile)]
status = DevSession::CLI.new(fixed + ARGV).run
AcceptedForkFixture.event('cli_finished', status:)
exit status
