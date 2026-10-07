# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'
require 'timeout'

class WorkspaceHostTest < Minitest::Test
  class StreamingClusterHost < DevWorkspaceHost::Host
    def initialize(provider:, **options)
      @provider = provider
      super(**options)
    end

    private

    def extension_catalog(_package = package_root)
      Struct.new(:provider_programs).new({
        'alpha-devcluster' => { 'command' => @provider }
      })
    end
  end

  def test_capture_lease_streams_readiness_input_and_stderr_under_generation_guard
    with_streaming_provider do |context|
      input, writer = IO.pipe
      reader, output = IO.pipe
      errors = StringIO.new
      original_channels = [input, output].map { |io| File.readlink("/proc/self/fd/#{io.fileno}") }
      host = streaming_host(context, input:, out: output, err: errors)
      result = Thread.new { host.run('alpha-devcluster', ['capture-lease', 'instance']) }
      ready = JSON.parse(Timeout.timeout(5) { reader.gets })
      assert_equal('ready', ready.fetch('event'))
      refute(ready.fetch('fds').any? { |target| original_channels.include?(target) })
      refute_includes(ready.fetch('fds'), context.fetch(:lock))
      assert_equal({}, ready.fetch('authority'))
      assert_equal(context.fetch(:workspace), ready.fetch('workspace'))
      assert_equal('example-workspace', ready.fetch('name'))
      File.open(context.fetch(:lock), File::RDWR) do |probe|
        refute(probe.flock(File::LOCK_EX | File::LOCK_NB))
      end
      writer.write("fixture-request\n")
      writer.flush
      assert_equal("echo:fixture-request\n", Timeout.timeout(5) { reader.gets })
      assert(result.alive?, 'readiness must precede provider exit')
      writer.close
      Timeout.timeout(5) { sleep 0.01 until File.exist?(context.fetch(:ended)) }
      File.open(context.fetch(:lock), File::RDWR) do |probe|
        refute(probe.flock(File::LOCK_EX | File::LOCK_NB), 'guard must cover provider cleanup')
      end
      assert_equal(0, Timeout.timeout(5) { result.value })
      assert_equal("provider diagnostics\n", errors.string)
      assert_equal('EOF', File.read(context.fetch(:ended)))
      assert_provider_reaped(context)
      File.open(context.fetch(:lock), File::RDWR) do |probe|
        assert(probe.flock(File::LOCK_EX | File::LOCK_NB))
      end
    ensure
      [input, writer, reader, output].compact.each { |io| io.close unless io.closed? }
      result&.kill&.join
    end
  end

  def test_capture_lease_early_failure_cancels_blocked_input_and_preserves_exit_mapping
    with_streaming_provider do |context|
      input, writer = IO.pipe
      output, errors = StringIO.new, StringIO.new
      host = streaming_host(context, input:, out: output, err: errors)
      assert_equal(1, Timeout.timeout(5) { host.run('alpha-devcluster', ['capture-lease', 'fail']) })
      assert_includes(output.string, 'early output')
      assert_includes(errors.string, 'early error')
      assert_includes(errors.string, 'command failed:')
      assert_provider_reaped(context)
    ensure
      [input, writer].compact.each { |io| io.close unless io.closed? }
    end
  end

  def test_capture_lease_early_success_cancels_blocked_input
    with_streaming_provider do |context|
      input, writer = IO.pipe
      host = streaming_host(context, input:, out: StringIO.new, err: StringIO.new)
      assert_equal(0, Timeout.timeout(5) { host.run('alpha-devcluster', ['capture-lease', 'exit']) })
      assert_provider_reaped(context)
    ensure
      [input, writer].compact.each { |io| io.close unless io.closed? }
    end
  end

  def test_capture_lease_broken_output_closes_provider_input_and_reaps_it
    with_streaming_provider do |context|
      input, writer = IO.pipe
      reader, output = IO.pipe
      reader.close
      errors = StringIO.new
      host = streaming_host(context, input:, out: output, err: errors)
      assert_equal(1, Timeout.timeout(5) { host.run('alpha-devcluster', ['capture-lease', 'instance']) })
      assert_includes(errors.string, 'development cluster relay failed: Errno::EPIPE')
      assert_equal('EOF', File.read(context.fetch(:ended)))
      assert_provider_reaped(context)
    ensure
      [input, writer, reader, output].compact.each { |io| io.close unless io.closed? }
    end
  end

  def test_capture_lease_interruption_finishes_owned_child_before_releasing_guard
    with_streaming_provider do |context|
      input, writer = IO.pipe
      reader, output = IO.pipe
      host = streaming_host(context, input:, out: output, err: StringIO.new)
      result = Thread.new do
        host.run('alpha-devcluster', ['capture-lease', 'instance'])
      rescue Interrupt
        :interrupted
      end
      Timeout.timeout(5) { reader.gets }
      result.raise(Interrupt)
      assert_equal(:interrupted, Timeout.timeout(5) { result.value })
      assert_equal('EOF', File.read(context.fetch(:ended)))
      assert_provider_reaped(context)
      File.open(context.fetch(:lock), File::RDWR) do |probe|
        assert(probe.flock(File::LOCK_EX | File::LOCK_NB))
      end
    ensure
      [input, writer, reader, output].compact.each { |io| io.close unless io.closed? }
      result&.kill&.join
    end
  end

  def test_capture_lease_abrupt_dispatcher_loss_closes_both_mediated_peer_channels
    with_streaming_provider do |context|
      input, writer = IO.pipe
      reader, output = IO.pipe
      dispatcher = fork do
        writer.close
        reader.close
        host = streaming_host(context, input:, out: output, err: StringIO.new)
        exit! host.run('alpha-devcluster', ['capture-lease', 'instance'])
      rescue StandardError => error
        warn "dispatcher fixture failed: #{error.class}: #{error.message}"
        exit! 1
      end
      input.close
      output.close
      ready = JSON.parse(Timeout.timeout(5) { reader.gets })
      assert_equal('ready', ready.fetch('event'))
      Process.kill('KILL', dispatcher)
      Process.waitpid(dispatcher)
      dispatcher = nil
      assert_nil(Timeout.timeout(5) { reader.gets }, 'caller must observe dispatcher loss')
      Timeout.timeout(5) { sleep 0.01 until File.exist?(context.fetch(:ended)) }
      assert_equal('EOF', File.read(context.fetch(:ended)), 'provider must observe input EOF')
      File.open(context.fetch(:lock), File::RDWR) do |probe|
        assert(probe.flock(File::LOCK_EX | File::LOCK_NB))
      end
    ensure
      if dispatcher
        Process.kill('KILL', dispatcher)
        Process.waitpid(dispatcher)
      end
      [input, writer, reader, output].compact.each { |io| io.close unless io.closed? }
    end
  end

  def test_capture_lease_cancellation_reaps_a_provider_that_ignores_eof_and_term
    with_streaming_provider do |context|
      input, writer = IO.pipe
      reader, output = IO.pipe
      host = streaming_host(context, input:, out: output, err: StringIO.new)
      result = Thread.new { host.run('alpha-devcluster', ['capture-lease', 'stall']) }
      Timeout.timeout(5) { reader.gets }
      writer.close
      assert_equal(1, Timeout.timeout(8) { result.value })
      assert_provider_reaped(context)
    ensure
      [input, writer, reader, output].compact.each { |io| io.close unless io.closed? }
      result&.kill&.join
    end
  end

  def test_waiting_capture_lease_rejects_a_changed_package_generation
    with_streaming_provider do |context|
      FileUtils.mkdir_p(File.dirname(context.fetch(:lock)))
      owner = File.open(context.fetch(:lock), File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      errors = StringIO.new
      host = streaming_host(context, input: StringIO.new, out: StringIO.new, err: errors)
      result = Thread.new { host.run('alpha-devcluster', ['capture-lease', 'instance']) }
      sleep 0.05
      refute(File.exist?(context.fetch(:pid)))
      profile = context.fetch(:environment).fetch('DEV_WORKSPACES_PROFILE')
      File.unlink(profile)
      File.symlink(context.fetch(:workspace), profile)
      owner.flock(File::LOCK_UN)
      assert_equal(1, Timeout.timeout(5) { result.value })
      assert_includes(errors.string, 'package transition completed')
      refute(File.exist?(context.fetch(:pid)))
    ensure
      owner&.close
      result&.kill&.join
    end
  end

  def test_non_streaming_provider_dispatch_retains_buffered_path
    with_streaming_provider do |context|
      host = streaming_host(context, input: StringIO.new, out: StringIO.new, err: StringIO.new)
      captured = nil
      host.define_singleton_method(:system_env!) { |*arguments| captured = arguments }
      assert_equal(0, host.run('alpha-devcluster', ['status', 'capture-lease']))
      assert_equal(['status', 'capture-lease'], captured.drop(2))
      refute(File.exist?(context.fetch(:pid)))
    end
  end

  def test_capture_lease_stdout_eof_closes_the_peer_while_input_stays_open_and_guard_covers_reap
    with_streaming_provider do |context|
      input, writer = IO.pipe
      reader, output = IO.pipe
      host = streaming_host(context, input:, out: output, err: StringIO.new)
      result = Thread.new { host.run('alpha-devcluster', ['capture-lease', 'stdout-ready']) }
      assert_equal('ready', JSON.parse(Timeout.timeout(5) { reader.gets })['event'])
      writer.puts('close-output')
      writer.flush
      assert_nil(Timeout.timeout(5) { reader.gets }, 'protocol EOF must reach the caller before provider exit')
      refute(writer.closed?, 'caller stdin deliberately remains open')
      Timeout.timeout(5) { sleep 0.01 until File.exist?(context.fetch(:ended)) }
      assert_equal('EOF', File.read(context.fetch(:ended)), 'provider stdin must close after stdout loss')
      File.open(context.fetch(:lock), File::RDWR) do |probe|
        refute(probe.flock(File::LOCK_EX | File::LOCK_NB), 'guard remains held during provider cleanup')
      end
      assert_equal(0, Timeout.timeout(5) { result.value })
      assert(output.closed?, 'injected protocol sink is closed too')
      assert_provider_reaped(context)
    ensure
      [input, writer, reader, output].compact.each { |io| io.close unless io.closed? }
      result&.kill&.join
    end
  end

  def test_capture_lease_stdout_eof_before_or_immediately_after_readiness_closes_injected_sink
    %w[stdout-before stdout-same].each do |mode|
      with_streaming_provider do |context|
        input, writer = IO.pipe
        output = StringIO.new
        host = streaming_host(context, input:, out: output, err: StringIO.new)
        assert_equal(0, Timeout.timeout(5) { host.run('alpha-devcluster', ['capture-lease', mode]) })
        assert(output.closed?, 'StringIO closure has the same protocol meaning as pipe EOF')
        assert_equal('EOF', File.read(context.fetch(:ended)))
        if mode == 'stdout-before'
          assert_empty(output.string)
        else
          assert_equal('ready', JSON.parse(output.string)['event'])
        end
        refute(writer.closed?)
        assert_provider_reaped(context)
      ensure
        [input, writer].compact.each { |io| io.close unless io.closed? }
      end
    end
  end

  def test_capture_lease_default_stdout_closes_its_os_descriptor_before_provider_cleanup
    with_streaming_provider do |context|
      catalog = context.fetch(:environment).fetch('DEV_WORKSPACES_EXTENSION_CATALOG')
      File.write(catalog, JSON.generate(
        'schema' => 1, 'commands' => [], 'skills' => [],
        'clusterProviders' => [
          { 'id' => 'alpha', 'label' => 'Alpha', 'command' => context.fetch(:provider) }
        ]
      ))
      release = context.fetch(:ended) + '.release'
      input, writer = IO.pipe
      reader, output = IO.pipe
      diagnostics = File.open(context.fetch(:ended) + '.diagnostics', 'w+')
      dispatcher = Process.spawn(
        context.fetch(:environment).merge(
          'DEV_WORKSPACE_HOST_MODE' => 'alpha-devcluster',
          'PROVIDER_PID' => context.fetch(:pid), 'PROVIDER_ENDED' => context.fetch(:ended),
          'PROVIDER_RELEASE' => release
        ),
        RbConfig.ruby, File.expand_path('../../libexec/workspace-host', __dir__),
        'capture-lease', 'stdout-default', in: input, out: output, err: diagnostics,
        close_others: true
      )
      input.close
      output.close
      assert_equal('ready', JSON.parse(Timeout.timeout(5) { reader.gets })['event'])
      writer.puts('close-output')
      writer.flush
      assert_nil(Timeout.timeout(5) { reader.gets }, 'default stdout must close its actual fd')
      refute(writer.closed?, 'caller input remains open after protocol loss')
      Timeout.timeout(5) { sleep 0.01 until File.exist?(context.fetch(:ended)) }
      provider_pid = Integer(File.read(context.fetch(:pid)))
      assert(File.exist?("/proc/#{provider_pid}"), 'provider cleanup has not finished')
      assert_nil(Process.waitpid(dispatcher, Process::WNOHANG))
      File.open(context.fetch(:lock), File::RDWR) do |probe|
        refute(probe.flock(File::LOCK_EX | File::LOCK_NB), 'generation guard covers cleanup')
      end
      File.write(release, 'finish')
      _, status = Timeout.timeout(5) { Process.waitpid2(dispatcher) }
      dispatcher = nil
      assert(status.success?)
      refute(File.exist?("/proc/#{provider_pid}"), 'dispatcher reaped its exact provider')
    ensure
      File.write(release, 'finish') if release
      if dispatcher
        Process.kill('KILL', dispatcher) rescue Errno::ESRCH
        Process.waitpid(dispatcher)
      end
      [input, writer, reader, output, diagnostics].compact.each { |io| io.close unless io.closed? }
    end
  end

  def test_capture_lease_stderr_eof_leaves_protocol_input_and_output_usable
    with_streaming_provider do |context|
      input, writer = IO.pipe
      reader, output = IO.pipe
      errors = StringIO.new
      host = streaming_host(context, input:, out: output, err: errors)
      result = Thread.new { host.run('alpha-devcluster', ['capture-lease', 'stderr-close']) }
      assert_equal('ready', JSON.parse(Timeout.timeout(5) { reader.gets })['event'])
      writer.puts('still-leased')
      writer.flush
      assert_equal("echo:still-leased\n", Timeout.timeout(5) { reader.gets })
      refute(output.closed?)
      refute(errors.closed?, 'diagnostic sink remains owned by the caller')
      assert(result.alive?)
      writer.close
      assert_equal(0, Timeout.timeout(5) { result.value })
      assert_provider_reaped(context)
    ensure
      [input, writer, reader, output].compact.each { |io| io.close unless io.closed? }
      result&.kill&.join
    end
  end

  private

  def with_streaming_provider
    Dir.mktmpdir('workspace-host-streaming') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      context = {
        workspace: root,
        provider: File.join(directory, 'provider'),
        pid: File.join(directory, 'provider.pid'),
        ended: File.join(directory, 'provider-ended'),
        lock: File.join(directory, 'state', 'transition.lock'),
        environment: install_source_profile(host_environment(directory, config:))
      }
      File.write(context.fetch(:provider), <<~RUBY)
        #!#{RbConfig.ruby}
        require 'json'
        def publish_input_eof
          target = ENV.fetch('PROVIDER_ENDED')
          temporary = target + '.tmp'
          # Existence must imply the complete EOF payload for waiting readers.
          File.open(temporary, 'wb') { |file| file.write('EOF') }
          File.rename(temporary, target)
        end
        File.write(ENV.fetch('PROVIDER_PID'), Process.pid.to_s)
        STDOUT.sync = true
        STDERR.sync = true
        if ARGV[1] == 'fail'
          puts 'early output'
          warn 'early error'
          exit 7
        end
        exit 0 if ARGV[1] == 'exit'
        if ARGV[1] == 'stdout-before'
          STDOUT.reopen(File::NULL, 'w')
          STDOUT.close
          STDIN.read
          publish_input_eof
          sleep 0.2
          exit 0
        end
        fds = Dir.glob('/proc/self/fd/*').filter_map do |path|
          File.readlink(path) rescue nil
        end
        authority = ENV.select { |name, _| name.match?(/TRANSITION_HELD|TRANSITION_LOCK_FD|LIFECYCLE/) }
        puts JSON.generate(event: 'ready', fds:, authority:,
                           workspace: ENV['DEVCLUSTER_WORKSPACE'], name: ENV['DEV_WORKSPACE_NAME'])
        warn 'provider diagnostics'
        if %w[stdout-ready stdout-same stdout-default].include?(ARGV[1])
          STDIN.gets unless ARGV[1] == 'stdout-same'
          STDOUT.reopen(File::NULL, 'w')
          STDOUT.close
          STDIN.read
          publish_input_eof
          if ARGV[1] == 'stdout-default'
            sleep 0.01 until File.exist?(ENV.fetch('PROVIDER_RELEASE'))
          else
            sleep 0.2
          end
          exit 0
        end
        if ARGV[1] == 'stderr-close'
          STDERR.reopen(File::NULL, 'w')
          STDERR.close
        end
        if ARGV[1] == 'stall'
          Signal.trap('TERM', 'IGNORE')
          STDIN.read
          loop { sleep 1 }
        end
        STDIN.each_line { |line| puts 'echo:' + line }
        publish_input_eof
        sleep 0.2
      RUBY
      File.chmod(0o755, context.fetch(:provider))
      yield context
    end
  end

  def streaming_host(context, **channels)
    environment = context.fetch(:environment).merge(
      'PROVIDER_PID' => context.fetch(:pid), 'PROVIDER_ENDED' => context.fetch(:ended),
      'DEV_WORKSPACE_TRANSITION_HELD' => '1', 'DEV_WORKSPACE_TRANSITION_LOCK_FD' => '999',
      'DEV_SESSION_LIFECYCLE_OPERATION' => 'archive',
      'DEV_SESSION_LIFECYCLE_LOCK_FD' => '998', 'DEV_SESSION_LIFECYCLE_LOCK_PATH' => '/ignored'
    )
    StreamingClusterHost.new(provider: context.fetch(:provider), env: environment, **channels)
  end

  def assert_provider_reaped(context)
    pid = Integer(File.read(context.fetch(:pid)))
    assert_raises(Errno::ECHILD) { Process.waitpid(pid, Process::WNOHANG) }
    refute(File.exist?("/proc/#{pid}"), 'exact spawned provider must be gone')
  end

  public

  def test_quiesce_ignores_a_manifest_from_another_codex_runtime
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      slug = '2026-09-06-old-runtime'
      manifest_dir = File.join(root, 'work', slug)
      FileUtils.mkdir_p(manifest_dir)
      File.write(
        File.join(manifest_dir, 'portal.yml'),
        portal_manifest('thread-old', '/run/old/app-server.sock', 'ready')
      )
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = QuiesceHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_empty(host.send(:quiesce_sessions))
      assert_empty(host.commands)
    end
  end

  def test_dev_session_dispatch_binds_the_registered_workspace_runtime
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime,
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      Dir.chdir(directory) do
        assert_equal(0, host.run('dev-session', ['--workspace', 'example-workspace', 'list']))
      end
      environment, command, arguments = host.captured
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal(File.join(directory, '.codex'), environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, root)
      assert_includes(arguments, File.join(runtime, 'example-workspace', 'app-server.sock'))
      lock_index = arguments.index('--transition-lock')
      assert_operator(lock_index, :<, arguments.index('--'))
      assert_equal(File.join(state, 'transition.lock'), arguments.fetch(lock_index + 1))
      token_index = arguments.index('--expected-host-profile-token')
      assert_operator(token_index, :<, arguments.index('--'))
      assert_match(/\A[0-9a-f]{64}\z/, arguments.fetch(token_index + 1))
      assert_equal('list', arguments.last)
    end
  end

  def test_ordinary_dispatch_passes_the_selected_codex_home_and_inherited_environment
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      selected_home = File.join(directory, 'selected-codex-home')
      configured_home = File.join(directory, 'codex-home')
      FileUtils.mkdir_p(selected_home)
      File.symlink(selected_home, configured_home)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      inherited = install_source_profile(host_environment(directory, config:)).merge(
        'CODEX_HOME' => configured_home,
        'DEV_WORKSPACE_CODEX_HOME' => File.join(directory, 'stale-codex-home'),
        'DEV_SESSION_SLUG' => '2026-09-06-test',
        'DEV_SESSION_WORKSPACE' => root,
        'DISPATCH_MARKER' => 'inherited-value'
      )
      host = CapturingHost.new(env: inherited, out: StringIO.new, err: StringIO.new)
      package = File.realpath(inherited.fetch('DEV_WORKSPACES_PROFILE'))
      expected_options = {
        '--workspace' => root,
        '--expected-host-generation' => package,
        '--host-profile' => inherited.fetch('DEV_WORKSPACES_PROFILE'),
        '--expected-host-profile-token' => host.send(:profile_link_token),
        '--transition-lock' => File.join(directory, 'state/transition.lock'),
        '--codex-socket' => File.join(directory, 'runtime/example-workspace/app-server.sock'),
        '--portal-command' => File.join(package, 'bin/workspace-portal')
      }

      [['list'], ['archive', '2026-09-06-test', '--as-is']].each do |request|
        assert_equal(0, host.run('dev-session', ['--workspace', 'example-workspace', *request]))
        environment, command, arguments = host.captured
        assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
        assert_equal(selected_home, environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
        inherited.each do |key, value|
          next if key == 'DEV_WORKSPACE_CODEX_HOME'

          assert_equal(value, environment.fetch(key), key)
        end
        assert_equal(File.join(package, 'libexec/workspace-portal/dev-session'), command)
        expected_options.each do |option, value|
          option_index = arguments.index(option)
          assert_operator(option_index, :<, arguments.index('--'), option)
          assert_equal(value, arguments.fetch(option_index + 1), option)
        end
        assert_equal(request, arguments.drop(arguments.index('--') + 1))
      end
    end
  end

  def test_exec_with_workspace_keeps_the_default_environment_for_existing_callers
    Dir.mktmpdir('workspace-host-test') do |directory|
      inherited = { 'HOME' => directory, 'DISPATCH_MARKER' => 'inherited-value' }
      host = CapturingHost.new(env: inherited, out: StringIO.new, err: StringIO.new)

      host.send(:exec_with_workspace, { 'name' => 'example-workspace' }, '/command', 'argument')

      assert_equal(
        [inherited.merge('DEV_WORKSPACE_NAME' => 'example-workspace'), '/command', ['argument']],
        host.captured
      )
    end
  end

  def test_cluster_commands_hold_the_shared_host_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      profile = File.join(directory, 'profile')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('../..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_PROFILE' => profile,
          'DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory)
        },
        out: StringIO.new,
        err: StringIO.new
      )
      result = Thread.new do
        host.run('alpha-devcluster', ['--workspace', 'example-workspace', 'status', '2026-09-06-test'])
      end
      sleep 0.05
      assert_nil(host.captured)
      owner.flock(File::LOCK_UN)

      assert_equal(0, result.value)
      refute_nil(host.captured)
    ensure
      owner&.close unless owner&.closed?
    end
  end

  def test_waiting_cluster_command_rejects_a_changed_package_generation
    %w[successful-switch compensated-switch].each do |scenario|
      Dir.mktmpdir("workspace-host-#{scenario}") do |directory|
        root = make_workspace(directory, 'workspace')
        config = File.join(directory, 'config', 'registry.json')
        state = File.join(directory, 'state')
        profile = File.join(directory, 'profile')
        expected = File.join(directory, 'old')
        selected = File.join(directory, 'new')
        FileUtils.mkdir_p(state)
        FileUtils.mkdir_p(expected)
        FileUtils.mkdir_p(selected)
        File.symlink(expected, profile)
        DevWorkspaceHost::Registry.new(config).register(
          name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
          aliases: [], replace: false
        )
        lock_path = File.join(state, 'transition.lock')
        owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        owner.flock(File::LOCK_EX)
        error_output = StringIO.new
        host = GenerationClusterHost.new(
          package_root: expected,
          env: {
            'HOME' => directory,
            'PATH' => ENV.fetch('PATH'),
            'DEV_WORKSPACES_CONFIG' => config,
            'DEV_WORKSPACES_STATE' => state,
            'DEV_WORKSPACES_PROFILE' => profile
          },
          out: StringIO.new,
          err: error_output
        )
        result = Thread.new do
          host.run('alpha-devcluster', [
            '--workspace', 'example-workspace', 'status', '2026-09-06-test'
          ])
        end
        sleep 0.05
        File.unlink(profile)
        File.symlink(selected, profile)
        if scenario == 'compensated-switch'
          File.unlink(profile)
          File.symlink(expected, profile)
        end
        owner.flock(File::LOCK_UN)

        assert_equal(1, result.value)
        assert_nil(host.captured)
        assert_includes(error_output.string, 'package transition completed')
      ensure
        owner&.flock(File::LOCK_UN)
        owner&.close
        result&.join
      end
    end
  end

  def test_waiting_host_mutation_rechecks_successful_and_compensated_switches
    %w[successful compensated].each do |scenario|
      Dir.mktmpdir("workspace-host-mutation-#{scenario}") do |directory|
        state = File.join(directory, 'state')
        profile = File.join(directory, 'profile')
        expected = File.join(directory, 'expected')
        candidate = File.join(directory, 'candidate')
        FileUtils.mkdir_p([state, expected, candidate])
        File.symlink(expected, profile)
        lock_path = File.join(state, 'transition.lock')
        owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        owner.flock(File::LOCK_EX)
        error_output = StringIO.new
        host = GenerationMutationHost.new(
          package_root: expected,
          env: {
            'HOME' => directory,
            'PATH' => ENV.fetch('PATH'),
            'DEV_WORKSPACES_STATE' => state,
            'DEV_WORKSPACES_PROFILE' => profile
          },
          out: StringIO.new,
          err: error_output
        )
        result = Thread.new { host.run('workspace-host', ['suspend']) }
        sleep 0.05
        assert_nil(host.captured)

        File.unlink(profile)
        File.symlink(candidate, profile)
        if scenario == 'compensated'
          File.unlink(profile)
          File.symlink(expected, profile)
        end
        owner.flock(File::LOCK_UN)

        assert_equal(1, result.value)
        assert_nil(host.captured)
        assert_includes(error_output.string, 'package transition completed')
      ensure
        owner&.flock(File::LOCK_UN)
        owner&.close
        result&.join
      end
    end
  end

  def test_cluster_command_ignores_spoofed_transition_and_lifecycle_environment
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      profile = File.join(directory, 'profile')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('../..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_PROFILE' => profile,
          'DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory),
          'DEV_WORKSPACE_TRANSITION_HELD' => '1',
          'DEV_SESSION_LIFECYCLE_OPERATION' => 'archive'
        },
        out: StringIO.new,
        err: StringIO.new
      )

      result = Thread.new do
        host.run(
          'alpha-devcluster',
          ['--workspace', 'example-workspace', 'reset', '2026-09-06-test']
        )
      end
      sleep 0.05
      assert_nil(host.captured)
      owner.flock(File::LOCK_UN)

      assert_equal(0, result.value)
      refute_nil(host.captured)
      environment, = host.captured
      refute(environment.key?('DEV_WORKSPACE_TRANSITION_HELD'))
      refute(environment.key?('DEV_SESSION_LIFECYCLE_OPERATION'))
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_public_delete_executes_the_cli_with_its_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => File.join(directory, 'state'),
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'example-workspace', 'delete', '2026-09-06-test', '--as-is']
        )
      )
      environment, command, arguments = host.captured
      refute(environment.key?('DEV_WORKSPACE_TRANSITION_HELD'))
      assert_equal('dev-session', File.basename(command))
      lock_index = arguments.index('--transition-lock')
      assert_operator(lock_index, :<, arguments.index('--'))
      assert_equal(File.join(directory, 'state', 'transition.lock'), arguments.fetch(lock_index + 1))
      assert_equal(
        ['delete', '2026-09-06-test', '--as-is'],
        arguments.last(3)
      )
    end
  end

  def test_portal_lifecycle_reuses_a_verified_inherited_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      selected_home = File.join(directory, 'selected-codex-home')
      configured_home = File.join(directory, 'codex-home')
      FileUtils.mkdir_p(selected_home)
      File.symlink(selected_home, configured_home)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      error_output = StringIO.new
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex,
          'CODEX_HOME' => configured_home,
          'DEV_WORKSPACE_CODEX_HOME' => File.join(directory, 'stale-codex-home'),
          'DISPATCH_MARKER' => 'inherited-value',
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: error_output
      )

      assert_equal(0, host.run(
        'dev-session',
        ['--workspace', 'example-workspace', 'delete', '2026-09-06-test', '--as-is']
      ), error_output.string)
      environment, command, arguments = host.captured
      assert_equal(owner.fileno.to_s, environment.fetch('DEV_WORKSPACE_TRANSITION_LOCK_FD'))
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal(selected_home, environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
      assert_equal(configured_home, environment.fetch('CODEX_HOME'))
      assert_equal('inherited-value', environment.fetch('DISPATCH_MARKER'))
      assert_equal('dev-session', File.basename(command))
      assert_equal(root, arguments.fetch(arguments.index('--workspace') + 1))
      refute_includes(arguments, '--transition-lock')
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_portal_lifecycle_keeps_inherited_shared_lock_and_supplies_ordinary_dispatch_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      selected_home = File.join(directory, 'selected-codex-home')
      configured_home = File.join(directory, 'codex-home')
      FileUtils.mkdir_p(selected_home)
      File.symlink(selected_home, configured_home)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_SH)
      error_output = StringIO.new
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex,
          'CODEX_HOME' => configured_home,
          'DEV_WORKSPACE_CODEX_HOME' => File.join(directory, 'stale-codex-home'),
          'DISPATCH_MARKER' => 'inherited-value',
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: error_output
      )

      assert_equal(0, host.run(
        'dev-session',
        ['--workspace', 'example-workspace', 'delete', '2026-09-06-test', '--as-is']
      ), error_output.string)
      environment, command, arguments = host.captured
      assert_equal(owner.fileno.to_s, environment.fetch('DEV_WORKSPACE_TRANSITION_LOCK_FD'))
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal(selected_home, environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
      assert_equal(configured_home, environment.fetch('CODEX_HOME'))
      assert_equal('inherited-value', environment.fetch('DISPATCH_MARKER'))
      assert_equal('dev-session', File.basename(command))
      assert_equal(root, arguments.fetch(arguments.index('--workspace') + 1))
      assert_includes(arguments, '--transition-lock')
      assert_equal(lock_path, arguments.fetch(arguments.index('--transition-lock') + 1))
      File.open(lock_path, File::RDWR) do |probe|
        assert(probe.flock(File::LOCK_SH | File::LOCK_NB))
        probe.flock(File::LOCK_UN)
        refute(probe.flock(File::LOCK_EX | File::LOCK_NB))
      end
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_inherited_transition_lock_rejects_an_unlocked_descriptor_for_the_same_inode
    Dir.mktmpdir('workspace-host-test') do |directory|
      state = File.join(directory, 'state')
      FileUtils.mkdir_p(state)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      impostor = File.open(lock_path, File::RDWR)
      host = DevWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => impostor.fileno.to_s
        },
        out: StringIO.new,
        err: StringIO.new
      )

      refute(host.send(:inherited_exclusive_transition_lock?))

      host = DevWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: StringIO.new
      )
      assert(host.send(:inherited_exclusive_transition_lock?))
    ensure
      impostor&.close
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_public_archive_delegates_cluster_cleanup_to_the_session_command
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'example-workspace', 'archive', '2026-09-06-test', '--as-is']
        )
      )
      environment, command, arguments = host.captured
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal(File.join(directory, '.codex'), environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, '--transition-lock')
      assert_equal(['archive', '2026-09-06-test', '--as-is'], arguments.last(3))
    end
  end

  def test_public_archive_passes_options_to_the_private_cli
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )
      argv = [
        '--workspace', 'example-workspace', 'archive', '--abandoned',
        '2026-09-06-Foo_bar', '--as-is'
      ]

      assert_equal(0, host.run('dev-session', argv))
      _environment, command, arguments = host.captured
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, '--transition-lock')
      assert_equal(['archive', '--abandoned', '2026-09-06-Foo_bar', '--as-is'], arguments.last(4))
    end
  end

end
