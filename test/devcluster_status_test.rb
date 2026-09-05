require 'json'
require 'digest'
require 'fileutils'
require 'minitest/autorun'
require 'open3'
require 'rbconfig'
require 'tmpdir'

class DevclusterStatusTest < Minitest::Test
  ROOT = File.expand_path('..', __dir__)
  HELPERS = {
    'vpsadmin' => File.join(ROOT, 'dev-clusters/vpsadmin/bin/devcluster'),
    'vpsadminos' => File.join(ROOT, 'dev-clusters/vpsadminos/bin/devcluster')
  }.freeze

  def test_vpsadmin_json_status_owns_links_commands_and_credentials
    with_cluster('vpsadmin') do |workspace, directory, slug|
      write_state(directory, 'topology', "single\n")
      write_state(directory, 'network', "local\n")
      write_state(directory, 'ready', '')
      write_state(directory, 'config.json', JSON.generate(
        'topologies' => { 'single' => %w[node1] },
        'domains' => { 'webui' => 'webui.example.test', 'auth' => 'auth.example.test' },
        'adminer' => { 'webAuth' => { 'username' => 'adminer', 'password' => 'secret' } },
        'seed' => { 'users' => [
          { 'login' => 'custom-user1', 'password' => 'custom-password1' },
          { 'login' => 'custom-user2', 'password' => 'custom-password2' }
        ] }
      ))

      status = read_status('vpsadmin', workspace, slug)
      assert_equal(1, status.fetch('schema'))
      assert_equal(true, status.fetch('found'))
      assert_equal('stale', status.fetch('state'))
      assert_equal(true, status.fetch('ready'))
      assert_equal('https://webui.example.test:10443/', status.fetch('links')[0].fetch('url'))
      assert_equal(%w[services node1], status.fetch('commands').map { |item| item.fetch('label') })
      assert_equal(8, status.fetch('credentials').length)
      assert_equal('custom-user1', status.fetch('credentials')[2].fetch('value'))
      assert_equal('secret', status.fetch('credentials').last.fetch('value'))

      stdout, stderr, result = Open3.capture3(
        { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
        HELPERS.fetch('vpsadmin'), 'urls', slug
      )
      assert(result.success?, stderr)
      status.fetch('links').each do |link|
        assert_includes(stdout, "#{link.fetch('label')}: #{link.fetch('url')}")
      end
      assert_includes(stdout, 'User login: custom-user1')
      assert_includes(stdout, 'User password: custom-password2')
      refute_includes(stdout, 'test-user1')
    end
  end

  def test_vpsadminos_json_status_owns_machine_commands
    with_cluster('vpsadminos') do |workspace, directory, slug|
      write_state(directory, 'topology', "dual\n")
      write_state(directory, 'network', "bridge\n")
      write_state(directory, 'config.json', JSON.generate(
        'topologies' => { 'dual' => %w[node1 node2] }
      ))

      status = read_status('vpsadminos', workspace, slug)
      assert_equal('stopped', status.fetch('state'))
      assert_equal(%w[node1 node2], status.fetch('commands').map { |item| item.fetch('label') })
      assert_empty(status.fetch('links'))
      assert_empty(status.fetch('credentials'))
    end
  end

  def test_missing_and_symlinked_cluster_state_fail_closed
    Dir.mktmpdir('devcluster-status') do |workspace|
      status = read_status('vpsadmin', workspace, 'missing')
      assert_equal(false, status.fetch('found'))

      root = File.join(workspace, '.dev-clusters/vpsadmin/clusters')
      FileUtils.mkdir_p(root)
      File.symlink(Dir.mktmpdir('devcluster-target'), File.join(root, 'unsafe'))
      _stdout, stderr, result = Open3.capture3(
        { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
        HELPERS.fetch('vpsadmin'), 'status', 'unsafe', '--json'
      )
      refute(result.success?)
      assert_includes(stderr, 'cluster state directory')
    end
  end

  def test_parent_symlinks_cannot_redirect_status_or_reset
    HELPERS.each_key do |kind|
      %w[state-root provider clusters].each do |component|
        assert_parent_symlink_fails_closed(kind, component)
      end
    end
  end

  def test_leaf_symlink_cannot_redirect_cluster_commands
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-leaf") do |workspace|
        slug = '2026-09-05-symlink-test'
        root = File.join(workspace, '.dev-clusters', kind, 'clusters')
        external = Dir.mktmpdir("devcluster-#{kind}-leaf-target")
        FileUtils.mkdir_p(root)
        File.write(File.join(external, 'ready'), 'keep-ready')
        File.write(File.join(external, 'result-config'), 'keep-result')
        File.symlink(external, File.join(root, slug))

        [
          ['status', slug],
          ['stop', slug],
          ['reset', slug],
          ['gcroots', '--cleanup', slug]
        ].each do |arguments|
          _stdout, stderr, result = Open3.capture3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace }, helper, *arguments
          )
          refute(result.success?, "#{kind} #{arguments.first} accepted a leaf symlink")
          assert_includes(stderr, 'cluster state directory')
          assert_equal('keep-ready', File.read(File.join(external, 'ready')))
          assert_equal('keep-result', File.read(File.join(external, 'result-config')))
        end
      ensure
        FileUtils.remove_entry(external) if external && File.exist?(external)
      end
    end
  end

  def test_queued_start_rechecks_session_lifecycle_after_taking_the_lock
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-lock') do |workspace|
        slug = '2026-09-05-lock-test'
        tracking = File.join(workspace, 'work', slug)
        lock_root = File.join(workspace, '.dev-clusters', '.locks')
        FileUtils.mkdir_p(tracking)
        FileUtils.mkdir_p(lock_root)
        File.write(File.join(tracking, 'state.md'), "---\nlifecycle: active\n---\n")
        lock_path = File.join(lock_root, "#{kind}-#{slug}.lock")
        File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
          lock.flock(File::LOCK_EX)
          stdin, stdout, stderr, waiter = Open3.popen3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
            helper, 'start', slug
          )
          stdin.close
          sleep 0.1
          assert(waiter.alive?, "#{kind} start did not wait for the lifecycle lock")
          File.write(File.join(tracking, 'state.md'), "---\nlifecycle: complete\n---\n")
          lock.flock(File::LOCK_UN)
          output = stdout.read
          error = stderr.read
          refute(waiter.value.success?, "#{kind} start unexpectedly succeeded: #{output}")
          assert_includes(error, "development session '#{slug}' is not active")
        end
        refute(File.exist?(File.join(workspace, '.dev-clusters', kind, 'clusters', slug)))
      end
    end
  end

  def test_state_producing_commands_reject_terminal_sessions
    commands = {
      'vpsadmin' => %w[config urls update],
      'vpsadminos' => %w[config update]
    }
    commands.each do |kind, operations|
      Dir.mktmpdir('devcluster-terminal') do |workspace|
        slug = '2026-09-05-terminal-test'
        write_lifecycle(workspace, slug, 'complete')
        operations.each do |operation|
          _stdout, stderr, result = Open3.capture3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
            HELPERS.fetch(kind), operation, slug
          )
          refute(result.success?, "#{kind} #{operation} accepted a terminal session")
          assert_includes(stderr, "development session '#{slug}' is not active")
        end
        refute(File.exist?(File.join(workspace, '.dev-clusters', kind, 'clusters', slug)))
      end
    end
  end

  def test_gcroot_cleanup_waits_for_the_session_lifecycle_lock
    HELPERS.each do |kind, helper|
      with_cluster(kind) do |workspace, directory, slug|
        lock_root = File.join(workspace, '.dev-clusters', '.locks')
        FileUtils.mkdir_p(lock_root)
        lock_path = File.join(lock_root, "#{kind}-#{slug}.lock")
        result_link = File.join(directory, 'result-config')
        File.symlink(File.join(workspace, 'result-target'), result_link)
        File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
          lock.flock(File::LOCK_EX)
          stdin, stdout, stderr, waiter = Open3.popen3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
            helper, 'gcroots', '--cleanup', slug
          )
          stdin.close
          sleep 0.1
          assert(waiter.alive?, "#{kind} gcroot cleanup did not wait for the lifecycle lock")
          lock.flock(File::LOCK_UN)
          output = stdout.read
          error = stderr.read
          assert(waiter.value.success?, "#{kind} gcroot cleanup failed: #{error}#{output}")
        end
        refute(File.exist?(result_link))
      end
    end
  end

  def test_detached_process_does_not_retain_the_lifecycle_lock
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-detach') do |workspace|
        slug = '2026-09-05-detach-test'
        runtime = File.join(ROOT, 'dev-clusters/lib/runtime.sh')
        child_pid_file = File.join(workspace, 'child.pid')
        script = <<~BASH
          set -euo pipefail
          WORKSPACE="$1"
          DEVCLUSTER_KIND="$2"
          STATE_ROOT="$WORKSPACE/.dev-clusters/$DEVCLUSTER_KIND"
          CHILD_PID_FILE="$4"
          SESSION_SLUG="$5"
          source "$3"
          die() { printf 'error: %s\\n' "$*" >&2; exit 1; }
          cluster_dir() { printf '%s/clusters/%s\\n' "$STATE_ROOT" "$1"; }
          callback() {
            local _slug="$1"
            (devcluster_exec_without_lifecycle_lock sleep 30) &
            printf '%s\\n' "$!" > "$CHILD_PID_FILE"
            wait
          }
          devcluster_with_lifecycle_lock "$SESSION_SLUG" false callback
        BASH
        parent = Process.spawn(
          'bash', '-c', script, 'bash', workspace, kind, runtime, child_pid_file, slug,
          out: File::NULL, err: File::NULL
        )
        child = wait_for_pid_file(child_pid_file)
        begin
          Process.kill('TERM', parent)
          Process.wait(parent)
          _stdout, stderr, result = Open3.capture3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace }, helper, 'reset', slug
          )
          assert(result.success?, "#{kind} reset remained blocked after launcher exit: #{stderr}")
          assert(Process.kill(0, child), "#{kind} detached child exited before lock verification")
        ensure
          stop_process(parent)
          stop_process(child)
        end
      end
    end
  end

  def test_process_lifecycle_implementation_is_shared
    HELPERS.each_value do |helper|
      refute_match(/^runner_process_matches\(\)/, File.read(helper))
      assert_includes(File.read(helper), 'source "$SCRIPT_DIR/../../lib/runtime.sh"')
    end
    runtime = File.read(File.join(ROOT, 'dev-clusters/lib/runtime.sh'))
    assert_equal(1, runtime.scan(/^runner_process_matches\(\)/).length)
    assert_equal(1, runtime.scan(/^kill_socket_processes\(\)/).length)
    %w[list_cluster_slugs remove_result_link gcroots_cluster gcroot_cluster].each do |function|
      HELPERS.each_value do |helper|
        refute_match(/^#{function}\(\)/, File.read(helper))
      end
      assert_equal(1, runtime.scan(/^#{function}\(\)/).length)
    end
  end

  def test_status_and_reset_match_the_exact_runner_socket_argument
    with_cluster('vpsadmin') do |workspace, directory, slug|
      marker = File.join('/tmp', "vpsfree-devcluster-#{Digest::SHA256.hexdigest(slug)[0, 12]}")
      write_state(directory, 'ready', '')
      write_state(directory, 'config.json', '{}')

      matching_pid = spawn_marker_process(marker)
      begin
        write_state(directory, 'runner.pid', "#{matching_pid}\n")
        assert_equal('running', read_status('vpsadmin', workspace, slug).fetch('state'))
      ensure
        stop_process(matching_pid)
      end

      unrelated_pid = spawn_marker_process("#{marker}-unrelated")
      begin
        write_state(directory, 'runner.pid', "#{unrelated_pid}\n")
        assert_equal('stale', read_status('vpsadmin', workspace, slug).fetch('state'))
        _stdout, stderr, result = Open3.capture3(
          { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
          HELPERS.fetch('vpsadmin'), 'reset', slug
        )
        assert(result.success?, stderr)
        assert(Process.kill(0, unrelated_pid))
        refute(File.exist?(directory))
      ensure
        stop_process(unrelated_pid)
      end
    end
  end

  private

  def with_cluster(kind)
    Dir.mktmpdir('devcluster-status') do |workspace|
      slug = '2026-09-05-test'
      directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
      FileUtils.mkdir_p(directory)
      write_lifecycle(workspace, slug, 'active')
      yield workspace, directory, slug
    end
  end

  def write_state(directory, name, content)
    File.write(File.join(directory, name), content)
  end

  def write_lifecycle(workspace, slug, lifecycle)
    directory = File.join(workspace, 'work', slug)
    FileUtils.mkdir_p(directory)
    File.write(File.join(directory, 'state.md'), "---\nlifecycle: #{lifecycle}\n---\n")
  end

  def read_status(kind, workspace, slug)
    stdout, stderr, result = Open3.capture3(
      { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
      HELPERS.fetch(kind), 'status', slug, '--json'
    )
    assert(result.success?, stderr)
    JSON.parse(stdout)
  end

  def assert_parent_symlink_fails_closed(kind, component)
    Dir.mktmpdir("devcluster-#{kind}-#{component}") do |workspace|
      slug = '2026-09-05-symlink-test'
      external = Dir.mktmpdir("devcluster-#{kind}-target")
      begin
        case component
        when 'state-root'
          target = File.join(external, kind, 'clusters', slug)
          FileUtils.mkdir_p(target)
          File.symlink(external, File.join(workspace, '.dev-clusters'))
        when 'provider'
          target = File.join(external, 'clusters', slug)
          FileUtils.mkdir_p(File.join(workspace, '.dev-clusters'))
          FileUtils.mkdir_p(target)
          File.symlink(external, File.join(workspace, '.dev-clusters', kind))
        when 'clusters'
          target = File.join(external, slug)
          FileUtils.mkdir_p(File.join(workspace, '.dev-clusters', kind))
          FileUtils.mkdir_p(target)
          File.symlink(external, File.join(workspace, '.dev-clusters', kind, 'clusters'))
        else
          raise "unknown parent component #{component}"
        end
        sentinel = File.join(target, 'sentinel')
        File.write(sentinel, 'keep')

        %w[status reset].each do |command|
          arguments = [HELPERS.fetch(kind), command, slug]
          arguments << '--json' if command == 'status'
          _stdout, stderr, result = Open3.capture3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace }, *arguments
          )
          refute(result.success?, "#{kind} #{command} accepted #{component} symlink")
          assert_includes(stderr, 'unsafe')
          assert_equal('keep', File.read(sentinel))
        end
      ensure
        FileUtils.remove_entry(external) if File.exist?(external)
      end
    end
  end

  def spawn_marker_process(marker)
    pid = Process.spawn(
      RbConfig.ruby, '-e', 'sleep 30', marker,
      out: File::NULL, err: File::NULL
    )
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 2
    loop do
      cmdline = File.read("/proc/#{pid}/cmdline").split("\0")
      return pid if cmdline.include?(marker)
      if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
        raise "marker process #{pid} did not start"
      end

      sleep 0.01
    end
  rescue StandardError
    stop_process(pid) if pid
    raise
  end

  def wait_for_pid_file(path)
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 2
    loop do
      if File.exist?(path)
        value = File.read(path).strip
        return Integer(value, 10) if value.match?(/\A[1-9][0-9]*\z/)
      end
      raise "process did not publish #{path}" if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline

      sleep 0.01
    end
  end

  def stop_process(pid)
    Process.kill('TERM', pid)
    Process.wait(pid)
  rescue Errno::ESRCH, Errno::ECHILD
    nil
  end
end
