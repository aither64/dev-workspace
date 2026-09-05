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
      assert_includes(stderr, 'unsafe cluster state directory')
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
      yield workspace, directory, slug
    end
  end

  def write_state(directory, name, content)
    File.write(File.join(directory, name), content)
  end

  def read_status(kind, workspace, slug)
    stdout, stderr, result = Open3.capture3(
      { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
      HELPERS.fetch(kind), 'status', slug, '--json'
    )
    assert(result.success?, stderr)
    JSON.parse(stdout)
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

  def stop_process(pid)
    Process.kill('TERM', pid)
    Process.wait(pid)
  rescue Errno::ESRCH, Errno::ECHILD
    nil
  end
end
