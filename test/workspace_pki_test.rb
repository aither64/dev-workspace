# frozen_string_literal: true

require 'fileutils'
require 'minitest/autorun'
require 'open3'
require 'rbconfig'
require 'tmpdir'

WORKSPACE_PKI_SCRIPT = File.expand_path('../bin/workspace-pki', __dir__)
load WORKSPACE_PKI_SCRIPT

class WorkspacePKITest < Minitest::Test
  SCRIPT = WORKSPACE_PKI_SCRIPT
  HOSTNAME = 'vpsfree-cz-workspace.aitherdev.int.vpsfree.cz'

  class FakeServiceManager
    attr_accessor :active, :fail_next_reload
    attr_reader :reloads

    def initialize(active: true)
      @active = active
      @fail_next_reload = false
      @reloads = 0
    end

    def active?(_service)
      @active
    end

    def reload(_service)
      @reloads += 1
      return unless @fail_next_reload

      @fail_next_reload = false
      raise WorkspacePKI::Error, 'simulated nginx reload failure'
    end
  end

  def test_initialization_creates_unencrypted_private_ca
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-unencrypted-test') do |directory|
      state = File.join(directory, 'state')
      stdout = run_success('init', '--state-dir', state)

      assert_includes(stdout, 'filesystem-protected CA')
      assert(key_readable_without_passphrase?(File.join(state, 'authority', 'ca-key.pem')))
      assert_equal(0o600, File.stat(File.join(state, 'authority', 'ca-key.pem')).mode & 0o777)
      run_success('renew', '--state-dir', state)
      run_success('verify', '--state-dir', state)
    end
  end

  def test_initialization_verification_renewal_and_public_export
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-test') do |directory|
      state = File.join(directory, 'state')
      run_success('init', '--state-dir', state)
      run_success('verify', '--state-dir', state)

      assert_equal(0o700, File.stat(state).mode & 0o777)
      assert_equal(0o600, File.stat(File.join(state, 'authority', 'ca-key.pem')).mode & 0o777)
      assert_equal(0o600, File.stat(File.join(state, 'current', 'server-key.pem')).mode & 0o777)
      assert(key_readable_without_passphrase?(File.join(state, 'authority', 'ca-key.pem')))

      original_path = File.realpath(File.join(state, 'current'))
      original = certificate_serial(File.join(original_path, 'server.pem'))
      run_success('renew', '--state-dir', state)
      replacement_path = File.realpath(File.join(state, 'current'))
      replacement = certificate_serial(File.join(replacement_path, 'server.pem'))
      refute_equal(original, replacement)
      refute_equal(original_path, replacement_path)
      assert(File.file?(File.join(original_path, 'server.pem')))
      assert(File.file?(File.join(original_path, 'server-key.pem')))
      run_success('verify', '--state-dir', state)

      exported = File.join(directory, 'export', 'workspace-ca.pem')
      run_success('export-ca', '--state-dir', state, exported)
      assert_equal(File.read(File.join(state, 'authority', 'ca.pem')), File.read(exported))
      assert_equal(0o644, File.stat(exported).mode & 0o777)
      runner = WorkspacePKI::Runner.new(
        state_dir: state,
        hostname: HOSTNAME,
        enforce_root: false,
        install_gid: Process.egid
      )
      export_directory = File.join(directory, 'existing-export-directory')
      FileUtils.mkdir_p(export_directory, mode: 0o755)
      error = assert_raises(WorkspacePKI::Error) { runner.export_ca(export_directory) }
      assert_includes(error.message, 'not a regular file')
      assert_equal(0o755, File.stat(export_directory).mode & 0o777)

      real_export = File.join(directory, 'real-export')
      linked_export = File.join(directory, 'linked-export')
      FileUtils.mkdir_p(real_export)
      File.symlink(real_export, linked_export)
      unsafe_destination = File.join(linked_export, 'created', 'ca.pem')
      error = assert_raises(WorkspacePKI::Error) { runner.export_ca(unsafe_destination) }
      assert_includes(error.message, 'symlink component')
      refute(File.exist?(File.join(real_export, 'created')))

      installed = File.join(directory, 'nginx')
      runner.install_server(installed)
      assert_equal(
        File.read(File.join(state, 'current', 'server.pem')),
        File.read(File.join(installed, 'current', 'server.pem'))
      )
      assert_equal(0o640, File.stat(File.join(installed, 'current', 'server-key.pem')).mode & 0o777)
      refute(File.exist?(File.join(installed, 'authority')))
      installed_target = File.readlink(File.join(installed, 'current'))
      runner.install_server(installed)
      assert_equal(installed_target, File.readlink(File.join(installed, 'current')))
      assert_equal(1, Dir.children(File.join(installed, 'pairs')).length)

      2.times do
        runner.renew
        runner.install_server(installed)
      end
      assert_equal(2, Dir.children(File.join(installed, 'pairs')).length)

      File.chmod(0o750, state)
      assert_raises(WorkspacePKI::Error) { runner.verify }
      assert_raises(WorkspacePKI::Error) { runner.inspect }
      assert_raises(WorkspacePKI::Error) do
        runner.export_ca(File.join(directory, 'drift-ca.pem'))
      end
      assert_raises(WorkspacePKI::Error) do
        runner.install_server(File.join(directory, 'drift-install'))
      end
    end
  end

  def test_failed_atomic_switch_preserves_the_current_pair
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-switch-test') do |directory|
      state = File.join(directory, 'state')
      run_success('init', '--state-dir', state)
      original_target = File.readlink(File.join(state, 'current'))
      original_versions = Dir.children(File.join(state, 'leaves')).sort

      failing_runner = Class.new(WorkspacePKI::Runner) do
        private

        def switch_current(*)
          raise WorkspacePKI::Error, 'simulated atomic switch failure'
        end
      end.new(
        state_dir: state,
        hostname: HOSTNAME,
        enforce_root: false
      )
      assert_raises(WorkspacePKI::Error) { failing_runner.renew }

      assert_equal(original_target, File.readlink(File.join(state, 'current')))
      assert_equal(original_versions, Dir.children(File.join(state, 'leaves')).sort)
      run_success('verify', '--state-dir', state)
    end
  end

  def test_concurrent_server_installations_are_serialized_by_the_cli_operation
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-concurrent-install-test') do |directory|
      state = File.join(directory, 'state')
      destination = File.join(directory, 'nginx')
      run_success('init', '--state-dir', state)
      mutex = Mutex.new
      active = 0
      maximum_active = 0
      runner_class = Class.new(WorkspacePKI::Runner) do
        define_method(:install_server_locked) do |output|
          mutex.synchronize do
            active += 1
            maximum_active = [maximum_active, active].max
          end
          sleep(0.1)
          super(output)
        ensure
          mutex.synchronize { active -= 1 }
        end
      end
      runner = runner_class.new(
        state_dir: state,
        hostname: HOSTNAME,
        enforce_root: false,
        install_gid: Process.egid
      )
      failures = Queue.new
      threads = 2.times.map do
        Thread.new do
          runner.install_server(destination)
        rescue StandardError => e
          failures << e
        end
      end
      threads.each(&:join)

      assert(failures.empty?, failures.empty? ? nil : failures.pop.full_message)
      assert_equal(1, maximum_active)
      assert_equal(1, Dir.children(File.join(destination, 'pairs')).length)
      assert(File.file?(File.join(destination, 'current', 'server.pem')))
      assert_equal(0o640, File.stat(File.join(destination, 'current', 'server-key.pem')).mode & 0o777)
    end
  end

  def test_interrupted_initialization_leaves_no_partial_state_and_can_retry
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-init-test') do |directory|
      state = File.join(directory, 'state')
      failing_runner = Class.new(WorkspacePKI::Runner) do
        private

        def publish_initial_state(*)
          raise WorkspacePKI::Error, 'simulated interrupted initialization'
        end
      end.new(state_dir: state, hostname: HOSTNAME, enforce_root: false)

      assert_raises(WorkspacePKI::Error) { failing_runner.init }
      refute(File.exist?(state))
      run_success('ensure', '--state-dir', state)
      run_success('verify', '--state-dir', state)
    end
  end

  def test_ensure_renews_before_expiry_and_is_repeatable
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-ensure-test') do |directory|
      state = File.join(directory, 'state')
      runner = WorkspacePKI::Runner.new(
        state_dir: state,
        hostname: HOSTNAME,
        enforce_root: false
      )
      runner.ensure
      original = certificate_serial(File.join(state, 'current', 'server.pem'))
      runner.ensure(renew_before: 400 * 24 * 60 * 60)
      replacement = certificate_serial(File.join(state, 'current', 'server.pem'))
      refute_equal(original, replacement)
      runner.ensure
      assert_equal(replacement, certificate_serial(File.join(state, 'current', 'server.pem')))
    end
  end

  def test_nginx_reconciliation_retries_after_public_ca_export_failure
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-reconcile-export-test') do |directory|
      service = FakeServiceManager.new
      runner = reconciliation_runner(directory, service)
      options = reconciliation_options(directory)
      runner.reconcile_nginx(**options)
      original_marker = File.binread(options.fetch(:applied_marker))
      runner.renew

      failing_class = Class.new(WorkspacePKI::Runner) do
        attr_accessor :fail_export

        def export_ca(path)
          raise WorkspacePKI::Error, 'simulated CA export failure' if fail_export

          super
        end
      end
      retrying_runner = reconciliation_runner(directory, service, runner_class: failing_class)
      retrying_runner.fail_export = true
      assert_raises(WorkspacePKI::Error) { retrying_runner.reconcile_nginx(**options) }
      assert_equal(original_marker, File.binread(options.fetch(:applied_marker)))
      assert_equal(1, service.reloads)

      retrying_runner.fail_export = false
      retrying_runner.reconcile_nginx(**options)
      refute_equal(original_marker, File.binread(options.fetch(:applied_marker)))
      assert_equal(2, service.reloads)
    end
  end

  def test_nginx_reconciliation_retries_reload_before_updating_marker_atomically
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-reconcile-reload-test') do |directory|
      service = FakeServiceManager.new
      runner = reconciliation_runner(directory, service)
      options = reconciliation_options(directory)
      runner.reconcile_nginx(**options)
      original_marker = File.binread(options.fetch(:applied_marker))
      runner.renew
      service.fail_next_reload = true

      assert_raises(WorkspacePKI::Error) { runner.reconcile_nginx(**options) }
      assert_equal(original_marker, File.binread(options.fetch(:applied_marker)))
      runner.reconcile_nginx(**options)

      marker = options.fetch(:applied_marker)
      target = File.realpath(File.join(options.fetch(:server_dir), 'current'))
      assert_equal("#{target}\n", File.binread(marker))
      assert_equal(0o600, File.stat(marker).mode & 0o777)
      assert_empty(Dir.glob("#{marker}.*"))
      assert_equal(3, service.reloads)
    end
  end

  def test_nginx_reconciliation_does_not_mark_an_inactive_service
    skip 'openssl is not available' unless command_available?('openssl')

    Dir.mktmpdir('workspace-pki-reconcile-inactive-test') do |directory|
      service = FakeServiceManager.new(active: false)
      runner = reconciliation_runner(directory, service)
      options = reconciliation_options(directory)
      runner.reconcile_nginx(**options)
      refute(File.exist?(options.fetch(:applied_marker)))
      assert_equal(0, service.reloads)

      service.active = true
      runner.reconcile_nginx(**options)
      assert(File.file?(options.fetch(:applied_marker)))
      assert_equal(1, service.reloads)
    end
  end

  def test_all_operations_require_root_by_default
    skip 'test runs as root' if Process.euid.zero?

    runner = WorkspacePKI::Runner.new(state_dir: '/tmp/not-used', hostname: HOSTNAME)
    error = assert_raises(WorkspacePKI::Error) { runner.verify }
    assert_includes(error.message, 'must run as root')

    _stdout, stderr, status = Open3.capture3(
      RbConfig.ruby, SCRIPT, 'init', '--state-dir', '/tmp/not-used'
    )
    refute(status.success?)
    assert_includes(stderr, 'must run as root')
  end

  def test_refuses_state_owned_by_an_unexpected_uid
    Dir.mktmpdir('workspace-pki-owner-test') do |directory|
      state = File.join(directory, 'state')
      FileUtils.mkdir_p(state, mode: 0o700)
      runner = WorkspacePKI::Runner.new(
        state_dir: state,
        hostname: HOSTNAME,
        state_uid: Process.euid + 1,
        enforce_root: false
      )

      error = assert_raises(WorkspacePKI::Error) { runner.init }
      assert_includes(error.message, 'must be owned by uid')
      refute(File.exist?(File.join(state, 'authority')))
    end
  end

  def test_refuses_to_store_private_keys_in_a_git_working_tree
    skip 'git is not available' unless command_available?('git')

    Dir.mktmpdir('workspace-pki-git-test') do |directory|
      _stdout, _stderr, status = Open3.capture3('git', 'init', '-q', directory)
      assert(status.success?)
      runner = WorkspacePKI::Runner.new(
        state_dir: File.join(directory, 'secrets'),
        hostname: HOSTNAME,
        enforce_root: false
      )
      error = assert_raises(WorkspacePKI::Error) { runner.init }
      assert_includes(error.message, 'outside a git working tree')
      refute(File.exist?(File.join(directory, 'secrets')))

    end
  end

  def test_refuses_symlinked_state_ancestors
    Dir.mktmpdir('workspace-pki-symlink-test') do |directory|
      real = File.join(directory, 'real')
      FileUtils.mkdir_p(real)
      linked = File.join(directory, 'linked')
      File.symlink(real, linked)
      runner = WorkspacePKI::Runner.new(
        state_dir: File.join(linked, 'state'),
        hostname: HOSTNAME,
        enforce_root: false
      )
      error = assert_raises(WorkspacePKI::Error) { runner.init }
      assert_includes(error.message, 'symlink component')
      refute(File.exist?(File.join(real, 'state')))
    end
  end

  def test_refuses_to_repermission_unmanaged_existing_directories
    Dir.mktmpdir('workspace-pki-unmanaged-test') do |directory|
      state = File.join(directory, 'state')
      FileUtils.mkdir_p(state, mode: 0o755)
      File.write(File.join(state, 'unrelated'), "keep\n")
      runner = WorkspacePKI::Runner.new(
        state_dir: state,
        hostname: HOSTNAME,
        enforce_root: false
      )
      error = assert_raises(WorkspacePKI::Error) { runner.init }
      assert_includes(error.message, 'PKI state already exists')
      assert_equal(0o755, File.stat(state).mode & 0o777)

      destination = File.join(directory, 'server')
      FileUtils.mkdir_p(destination, mode: 0o755)
      File.write(File.join(destination, 'unrelated'), "keep\n")
      error = assert_raises(WorkspacePKI::Error) do
        runner.send(
          :claim_managed_directory!, destination, WorkspacePKI::SERVER_MARKER,
          'server certificate destination'
        )
      end
      assert_includes(error.message, 'not an empty mode-0700 directory')
      assert_equal(0o755, File.stat(destination).mode & 0o777)
    end
  end

  private

  def reconciliation_runner(directory, service, runner_class: WorkspacePKI::Runner)
    runner_class.new(
      state_dir: File.join(directory, 'state'),
      hostname: HOSTNAME,
      enforce_root: false,
      install_gid: Process.egid,
      service_manager: service
    )
  end

  def reconciliation_options(directory)
    {
      server_dir: File.join(directory, 'nginx'),
      public_ca: File.join(directory, 'public', 'ca.pem'),
      applied_marker: File.join(directory, 'nginx', '.nginx-applied'),
      lock_file: File.join(directory, 'lock', 'reconcile.lock')
    }
  end

  def run_success(*arguments)
    command = arguments.shift
    state_dir = extract_option(arguments, '--state-dir') || WorkspacePKI::STATE_DIR
    hostname = extract_option(arguments, '--hostname') || HOSTNAME
    runner = WorkspacePKI::Runner.new(state_dir:, hostname:, enforce_root: false)
    method = command.tr('-', '_')
    stdout, = capture_io do
      if %w[export-ca install-server].include?(command)
        runner.public_send(method, arguments.fetch(0))
      else
        runner.public_send(method)
      end
    end
    stdout
  end

  def extract_option(arguments, option)
    index = arguments.index(option)
    return unless index

    arguments.slice!(index, 2).fetch(1)
  end

  def certificate_serial(path)
    stdout, stderr, status = Open3.capture3('openssl', 'x509', '-in', path, '-noout', '-serial')
    assert(status.success?, stderr)
    stdout.strip
  end

  def key_readable_without_passphrase?(path)
    _stdout, _stderr, status = Open3.capture3(
      'openssl', 'pkey', '-in', path, '-noout', '-passin', 'pass:'
    )
    status.success?
  end

  def command_available?(name)
    ENV.fetch('PATH', '').split(File::PATH_SEPARATOR).any? do |directory|
      File.executable?(File.join(directory, name))
    end
  end
end
