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

  def test_vpsadmin_certificate_state_is_locked_and_rejects_symlinks
    Dir.mktmpdir('devcluster-cert-lock') do |workspace|
      external = Dir.mktmpdir('devcluster-cert-target')
      certs = File.join(workspace, '.dev-clusters', 'vpsadmin', 'certs')
      lock_root = File.join(workspace, '.dev-clusters', '.locks')
      FileUtils.mkdir_p(File.dirname(certs))
      FileUtils.mkdir_p(lock_root)
      FileUtils.mkdir_p(File.join(external, 'default'))
      sentinel = File.join(external, 'default', 'vpsadmin-ca.keep')
      File.write(sentinel, 'keep')
      File.symlink(external, certs)

      lock_path = File.join(lock_root, 'vpsadmin-credentials.lock')
      File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
        lock.flock(File::LOCK_EX)
        stdin, stdout, stderr, waiter = Open3.popen3(
          { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
          HELPERS.fetch('vpsadmin'), 'cert', 'init', '--force'
        )
        stdin.close
        sleep 0.1
        assert(waiter.alive?, 'certificate initialization did not wait for the provider lock')
        lock.flock(File::LOCK_UN)
        output = stdout.read
        error = stderr.read
        refute(waiter.value.success?, "certificate initialization unexpectedly succeeded: #{output}")
        assert_includes(error, 'unsafe vpsadmin certificate state directory')
      end
      assert_equal('keep', File.read(sentinel))
    ensure
      FileUtils.remove_entry(external) if external && File.exist?(external)
    end
  end

  def test_vpsadmin_certificate_files_cannot_redirect_writes
    Dir.mktmpdir('devcluster-cert-file') do |workspace|
      cert_dir = File.join(workspace, '.dev-clusters', 'vpsadmin', 'certs', 'default')
      external = File.join(workspace, 'external-key')
      FileUtils.mkdir_p(cert_dir)
      File.write(external, 'keep')
      File.symlink(external, File.join(cert_dir, 'vpsadmin-ca.key'))

      _stdout, stderr, result = Open3.capture3(
        { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
        HELPERS.fetch('vpsadmin'), 'cert', 'init', '--force'
      )
      refute(result.success?)
      assert_includes(stderr, 'unsafe vpsadmin certificate state file')
      assert_equal('keep', File.read(external))
    end
  end

  def test_vpsadmin_certificate_leaf_cannot_redirect_writes
    Dir.mktmpdir('devcluster-cert-leaf') do |workspace|
      certs = File.join(workspace, '.dev-clusters', 'vpsadmin', 'certs')
      external = Dir.mktmpdir('devcluster-cert-leaf-target')
      FileUtils.mkdir_p(certs)
      sentinel = File.join(external, 'vpsadmin-ca.keep')
      File.write(sentinel, 'keep')
      File.symlink(external, File.join(certs, 'default'))

      _stdout, stderr, result = Open3.capture3(
        { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
        HELPERS.fetch('vpsadmin'), 'cert', 'init', '--force'
      )
      refute(result.success?)
      assert_includes(stderr, 'unsafe vpsadmin certificate set directory')
      assert_equal('keep', File.read(sentinel))
    ensure
      FileUtils.remove_entry(external) if external && File.exist?(external)
    end
  end

  def test_shared_ssh_directory_is_locked_and_cannot_redirect_key_generation
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-ssh-leaf") do |workspace|
        slug = '2026-09-05-ssh-symlink-test'
        provider_root = File.join(workspace, '.dev-clusters', kind)
        lock_root = File.join(workspace, '.dev-clusters', '.locks')
        external = Dir.mktmpdir("devcluster-#{kind}-ssh-leaf-target")
        FileUtils.mkdir_p(provider_root)
        FileUtils.mkdir_p(lock_root)
        File.symlink(external, File.join(provider_root, 'ssh'))
        write_lifecycle(workspace, slug, 'active')

        lock_path = File.join(lock_root, "#{kind}-credentials.lock")
        File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
          lock.flock(File::LOCK_EX)
          stdin, stdout, stderr, waiter = Open3.popen3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
            helper, 'start', slug, '--network', 'local'
          )
          stdin.close
          sleep 2.5
          assert(waiter.alive?, "#{kind} key generation did not wait for the provider lock")
          lock.flock(File::LOCK_UN)
          output = stdout.read
          error = stderr.read
          refute(waiter.value.success?, "#{kind} start unexpectedly succeeded: #{output}")
          assert_includes(error, "unsafe #{kind} SSH state directory")
        end
        assert_empty(Dir.children(external))
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

  def test_cluster_start_refuses_an_unfinished_session_lifecycle_operation
    runtime_contract.fetch('lifecycleJournals').each do |entry|
      operation = entry.fetch('name')
      HELPERS.each do |kind, helper|
        Dir.mktmpdir("devcluster-#{kind}-#{operation}") do |workspace|
          slug = '2026-09-05-changing'
          write_lifecycle(workspace, slug, 'active')
          lock_root = File.join(workspace, 'worktrees', '.locks')
          FileUtils.mkdir_p(lock_root)
          journal = File.join(lock_root, "#{slug}.#{operation}.json")
          File.write(journal, "{}\n")
          File.chmod(0o600, journal)

          _stdout, stderr, result = Open3.capture3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace }, helper, 'start', slug
          )

          refute(result.success?, "#{kind} started during session #{operation}")
          assert_includes(stderr, 'unfinished lifecycle operation')
        end
      end
    end
  end

  def test_cluster_helpers_accept_tracking_state_above_one_mibibyte
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-tracking-limit") do |workspace|
        slug = '2026-09-05-large-tracking'
        write_lifecycle(workspace, slug, 'active')
        state = File.join(workspace, 'work', slug, 'state.md')
        File.open(state, 'a') { |file| file.write('x' * (2 * 1024 * 1024)) }

        _stdout, stderr, result = Open3.capture3(
          { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace }, helper, 'config', slug
        )

        assert(result.success?, "#{kind} rejected shared tracking limit: #{stderr}")
      end
    end
  end

  def test_all_cluster_mutations_refuse_unfinished_lifecycle_operations
    commands = {
      'vpsadmin' => [
        %w[stop], %w[reset], %w[refresh], %w[restart api], %w[ssh webui],
        %w[config], %w[urls], %w[update], %w[transition-adopt]
      ],
      'vpsadminos' => [
        %w[stop], %w[reset], %w[restart osctl], %w[ssh tank],
        %w[config], %w[update], %w[transition-adopt]
      ]
    }
    commands.each do |kind, invocations|
      Dir.mktmpdir("devcluster-#{kind}-mutations") do |workspace|
        slug = '2026-09-05-changing'
        write_lifecycle(workspace, slug, 'active')
        lock_root = File.join(workspace, 'worktrees', '.locks')
        FileUtils.mkdir_p(lock_root)
        journal = File.join(lock_root, "#{slug}.archive.json")
        File.write(journal, "{}\n")
        File.chmod(0o600, journal)

        invocations.each do |arguments|
          _stdout, stderr, result = Open3.capture3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
            HELPERS.fetch(kind), arguments.fetch(0), slug, *arguments.drop(1)
          )
          refute(result.success?, "#{kind} #{arguments.first} mutated during archive")
          assert_includes(stderr, 'unfinished lifecycle operation')
        end
      end
    end
  end

  def test_lifecycle_operation_name_without_the_owned_lock_cannot_reset_a_cluster
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-owned-reset") do |workspace|
        slug = '2026-09-05-changing'
        write_lifecycle(workspace, slug, 'active')
        lock_root = File.join(workspace, 'worktrees', '.locks')
        FileUtils.mkdir_p(lock_root)
        journal = File.join(lock_root, "#{slug}.archive.json")
        File.write(journal, "{}\n")
        File.chmod(0o600, journal)

        _stdout, stderr, result = Open3.capture3(
          {
            'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace,
            'VPSFREE_DEV_SESSION_LIFECYCLE_OPERATION' => 'archive'
          },
          helper, 'reset', slug
        )
        refute(result.success?, "#{kind} accepted an unowned lifecycle operation")
        assert_includes(stderr, 'lifecycle lock is unavailable')
      end
    end
  end

  def test_lifecycle_reset_rejects_an_unlocked_descriptor_for_the_canonical_lock
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-lock-owner") do |workspace|
        slug = '2026-09-05-changing'
        directory, lock_path = prepare_lifecycle_reset(workspace, kind, slug)
        owner = File.open(lock_path, File::RDWR)
        owner.flock(File::LOCK_EX)
        impostor = File.open(lock_path, File::RDWR)

        _stdout, stderr, result = run_lifecycle_reset(
          helper, workspace, slug, impostor, lock_path
        )

        refute(result.success?, "#{kind} accepted an unlocked same-inode descriptor")
        assert_includes(stderr, 'does not own the exclusive lock')
        assert(File.exist?(File.join(directory, 'sentinel')))
      ensure
        impostor&.close
        owner&.flock(File::LOCK_UN)
        owner&.close
      end
    end
  end

  def test_lifecycle_reset_rejects_a_locked_noncanonical_path
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-lock-path") do |workspace|
        slug = '2026-09-05-changing'
        directory, = prepare_lifecycle_reset(workspace, kind, slug)
        arbitrary_root = File.join(workspace, 'arbitrary')
        FileUtils.mkdir_p(arbitrary_root)
        lock_path = File.join(arbitrary_root, "#{slug}.lock")
        lock = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        File.chmod(0o600, lock_path)
        lock.flock(File::LOCK_EX)

        _stdout, stderr, result = run_lifecycle_reset(
          helper, workspace, slug, lock, lock_path
        )

        refute(result.success?, "#{kind} accepted a noncanonical lifecycle lock")
        assert_includes(stderr, "does not belong to '#{slug}'")
        assert(File.exist?(File.join(directory, 'sentinel')))
      ensure
        lock&.flock(File::LOCK_UN)
        lock&.close
      end
    end
  end

  def test_lifecycle_reset_accepts_the_descriptor_that_owns_the_canonical_lock
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-lock-positive") do |workspace|
        slug = '2026-09-05-changing'
        directory, lock_path = prepare_lifecycle_reset(workspace, kind, slug)
        lock = File.open(lock_path, File::RDWR)
        lock.flock(File::LOCK_EX)

        _stdout, stderr, result = run_lifecycle_reset(
          helper, workspace, slug, lock, lock_path
        )

        assert(result.success?, stderr)
        refute(File.exist?(directory))
      ensure
        lock&.flock(File::LOCK_UN)
        lock&.close
      end
    end
  end

  def test_lifecycle_reset_accepts_a_custom_workspace_runtime_authority_lock
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-custom-runtime") do |workspace|
        slug = '2026-09-05-changing'
        directory, = prepare_lifecycle_reset(workspace, kind, slug)
        runtime_root = File.join(workspace, 'custom-runtime')
        authority = File.join(
          runtime_root, 'vpsfree-cz', 'authority'
        )
        FileUtils.mkdir_p(authority)
        lock_path = File.join(authority, "#{slug}.lock")
        lock = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        File.chmod(0o600, lock_path)
        lock.flock(File::LOCK_EX)

        _stdout, stderr, result = run_lifecycle_reset(
          helper,
          workspace,
          slug,
          lock,
          lock_path,
          'VPSFREE_WORKSPACE_NAME' => 'vpsfree-cz',
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => runtime_root
        )

        assert(result.success?, stderr)
        refute(File.exist?(directory))
      ensure
        lock&.flock(File::LOCK_UN)
        lock&.close
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
          devcluster_with_lifecycle_lock "$SESSION_SLUG" false true callback
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
      refute_match(/^devcluster_socket_dir\(\)/, File.read(helper))
      assert_includes(File.read(helper), 'source "$SCRIPT_DIR/../../lib/runtime.sh"')
      assert_includes(File.read(helper), 'remove_cluster_runtime_state "$slug" "$sock_dir"')
    end
    runtime = File.read(File.join(ROOT, 'dev-clusters/lib/runtime.sh'))
    assert_equal(1, runtime.scan(/^runner_process_matches\(\)/).length)
    assert_equal(1, runtime.scan(/^devcluster_socket_dir\(\)/).length)
    assert_equal(1, runtime.scan(/^signal_cluster_runner\(\)/).length)
    assert_equal(1, runtime.scan(/^kill_socket_processes\(\)/).length)
    %w[
      list_cluster_slugs remove_cluster_runtime_state remove_result_link
      gcroots_cluster gcroot_cluster
    ].each do |function|
      HELPERS.each_value do |helper|
        refute_match(/^#{function}\(\)/, File.read(helper))
      end
      assert_equal(1, runtime.scan(/^#{function}\(\)/).length)
    end
  end

  def test_privileged_start_is_not_part_of_the_managed_contract
    HELPERS.each_value do |helper|
      source = File.read(helper)
      refute_includes(source, '--sudo')
      refute_includes(source, 'DEVCLUSTER_USE_SUDO')
      refute_includes(source, 'sudo -E')
    end
  end

  def test_status_and_reset_match_the_exact_runner_socket_argument
    with_cluster('vpsadmin') do |workspace, directory, slug|
      marker = workspace_socket_directory('vpsadmin', workspace, slug)
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

  def test_reset_kills_children_that_reference_socket_files
    HELPERS.each_key do |kind|
      with_cluster(kind) do |workspace, _directory, slug|
        socket_directory = workspace_socket_directory(kind, workspace, slug)
        arguments = [
          "socket,id=char0,path=#{socket_directory}/machine.sock,server=on",
          "--socket-path=#{socket_directory}/virtiofs.sock"
        ]
        children = arguments.map { |argument| spawn_marker_process(argument) }
        unrelated = spawn_marker_process("--socket-path=#{socket_directory}-other/virtiofs.sock")

        begin
          _stdout, stderr, result = Open3.capture3(
            { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace },
            HELPERS.fetch(kind), 'reset', slug
          )
          assert(result.success?, stderr)
          children.each do |pid|
            assert_process_exited(pid, "#{kind} left socket child #{pid} running")
          end
          assert(Process.kill(0, unrelated), "#{kind} killed a prefix-matching process")
        ensure
          children.each { |pid| stop_process(pid) }
          stop_process(unrelated)
        end
      end
    end
  end

  def test_reset_does_not_kill_a_same_slug_cluster_from_another_workspace
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-workspace-a") do |workspace_a|
        Dir.mktmpdir("devcluster-#{kind}-workspace-b") do |workspace_b|
          slug = '2026-09-07-shared-slug'
          directory_a = File.join(workspace_a, '.dev-clusters', kind, 'clusters', slug)
          directory_b = File.join(workspace_b, '.dev-clusters', kind, 'clusters', slug)
          FileUtils.mkdir_p(directory_a)
          FileUtils.mkdir_p(directory_b)
          socket_b = workspace_socket_directory(kind, workspace_b, slug)
          child = spawn_marker_process("--socket-path=#{socket_b}/machine.sock")
          begin
            File.write(File.join(directory_b, 'runner.pid'), "#{child}\n")
            _stdout, stderr, result = Open3.capture3(
              { 'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace_a }, helper, 'reset', slug
            )
            assert(result.success?, stderr)
            assert(Process.kill(0, child), "#{kind} reset killed another workspace's cluster")
            assert(File.directory?(directory_b))
          ensure
            stop_process(child)
          end
        end
      end
    end
  end

  def test_cleanup_path_contract_comes_from_each_provider
    HELPERS.each do |kind, helper|
      with_cluster(kind) do |workspace, directory, slug|
        stdout, stderr, result = Open3.capture3(
          {'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace}, helper, 'cleanup-paths', slug
        )
        assert(result.success?, stderr)
        contract = JSON.parse(stdout)
        assert_equal(1, contract.fetch('schema'))
        assert_equal(
          [directory, workspace_socket_directory(kind, workspace, slug)],
          contract.fetch('paths')
        )
      end
    end
  end

  def test_vpsadmin_cleanup_path_contract_includes_the_one_legacy_socket
    kind = 'vpsadmin'
    slug = '2026-08-18-vpsadmin-password-reset'
    Dir.mktmpdir('devcluster-cleanup-contract') do |workspace|
      stdout, stderr, result = Open3.capture3(
        {'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace},
        HELPERS.fetch(kind), 'cleanup-paths', slug
      )
      assert(result.success?, stderr)
      contract = JSON.parse(stdout)
      legacy = "/tmp/vpsfree-devcluster-#{Digest::SHA256.hexdigest(slug)[0, 12]}"
      assert_equal(
        [
          File.join(workspace, '.dev-clusters', kind, 'clusters', slug),
          workspace_socket_directory(kind, workspace, slug),
          legacy
        ],
        contract.fetch('paths')
      )
    end
  end

  def test_package_transition_rejects_generic_precontract_cluster_state
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-transition-contract') do |workspace|
        slug = '2026-09-07-generic-cluster'
        FileUtils.mkdir_p(
          File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
        )

        _stdout, stderr, result = Open3.capture3(
          {'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace},
          helper, 'transition-adopt', slug
        )

        refute(result.success?)
        assert_includes(stderr, 'has no recorded socket identity')
      end
    end
  end

  def test_package_transition_accepts_workspace_scoped_cluster_state
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-transition-contract') do |workspace|
        slug = '2026-09-07-workspace-cluster'
        directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
        FileUtils.mkdir_p(directory)
        File.write(
          File.join(directory, 'socket-dir'),
          "#{workspace_socket_directory(kind, workspace, slug)}\n"
        )

        _stdout, stderr, result = Open3.capture3(
          {'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace},
          helper, 'transition-adopt', slug
        )

        assert(result.success?, stderr)
      end
    end
  end

  def test_package_transition_refuses_stale_precontract_state_even_with_legacy_opt_in
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-transition-stale') do |workspace|
        slug = "stale-#{kind}-#{Process.pid}"
        directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
        prefix = kind == 'vpsadmin' ? 'vpsfree-devcluster' : 'vpsadminos-devcluster'
        legacy = "/tmp/#{prefix}-#{Digest::SHA256.hexdigest(slug)[0, 12]}"
        FileUtils.mkdir_p(directory)

        _stdout, stderr, result = Open3.capture3(
          {
            'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace,
            'VPSFREE_DEVCLUSTER_ALLOW_PRECONTRACT_ADOPTION' => '1'
          },
          helper, 'transition-adopt', slug
        )

        refute(result.success?)
        assert_includes(stderr, 'has no recorded socket identity')
        canonical = workspace_socket_directory(kind, workspace, slug)
        refute(File.exist?(File.join(directory, 'socket-dir')))
        refute(File.exist?(legacy))
        refute(File.exist?(File.join(directory, 'legacy-socket-owner')))
        refute(File.exist?(canonical))
      ensure
        FileUtils.rm_rf(legacy) if legacy
      end
    end
  end

  def test_package_transition_refuses_an_existing_unreferenced_legacy_socket_directory
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-transition-open-legacy') do |workspace|
        slug = "open-legacy-#{kind}-#{Process.pid}"
        directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
        prefix = kind == 'vpsadmin' ? 'vpsfree-devcluster' : 'vpsadminos-devcluster'
        legacy = "/tmp/#{prefix}-#{Digest::SHA256.hexdigest(slug)[0, 12]}"
        FileUtils.mkdir_p(directory)
        FileUtils.mkdir_p(legacy)
        held = File.open(File.join(legacy, 'held'), File::WRONLY | File::CREAT, 0o600)

        _stdout, stderr, result = Open3.capture3(
          {
            'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace,
            'VPSFREE_DEVCLUSTER_ALLOW_PRECONTRACT_ADOPTION' => '1'
          },
          helper, 'transition-adopt', slug
        )

        refute(result.success?)
        assert_includes(stderr, 'has no recorded socket identity')
        assert(File.directory?(legacy))
        refute(File.exist?(File.join(directory, 'socket-dir')))
      ensure
        held&.close
        FileUtils.rm_rf(legacy) if legacy
      end
    end
  end

  def test_package_transition_refuses_unproven_live_precontract_state
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-transition-live') do |workspace|
        slug = "unproven-#{kind}-#{Process.pid}"
        directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
        prefix = kind == 'vpsadmin' ? 'vpsfree-devcluster' : 'vpsadminos-devcluster'
        legacy = "/tmp/#{prefix}-#{Digest::SHA256.hexdigest(slug)[0, 12]}"
        FileUtils.mkdir_p(directory)
        FileUtils.mkdir_p(legacy)
        child = spawn_marker_process('--sock-dir', legacy)
        begin
          _stdout, stderr, result = Open3.capture3(
            {
              'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace,
              'VPSFREE_DEVCLUSTER_ALLOW_PRECONTRACT_ADOPTION' => '1'
            },
            helper, 'transition-adopt', slug
          )

          refute(result.success?)
          assert_includes(stderr, 'has no recorded socket identity')
          refute(File.exist?(File.join(directory, 'socket-dir')))
          refute(File.exist?(File.join(directory, 'legacy-socket-owner')))
          assert(Process.kill(0, child))
        ensure
          stop_process(child)
        end
      ensure
        FileUtils.rm_rf(legacy) if legacy
      end
    end
  end

  def test_package_transition_refuses_generic_matching_legacy_runner
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-transition-generic-runner') do |workspace|
        slug = "generic-runner-#{kind}-#{Process.pid}"
        directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
        prefix = kind == 'vpsadmin' ? 'vpsfree-devcluster' : 'vpsadminos-devcluster'
        legacy = "/tmp/#{prefix}-#{Digest::SHA256.hexdigest(slug)[0, 12]}"
        FileUtils.mkdir_p(directory)
        child = spawn_marker_process(
          '--sock-dir', legacy,
          '--state-dir', File.join(directory, 'state'),
          '--pid-file', File.join(directory, 'runner.pid'),
          '--ready-file', File.join(directory, 'ready')
        )
        File.write(File.join(directory, 'runner.pid'), "#{child}\n")
        begin
          _stdout, stderr, result = Open3.capture3(
            {
              'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace,
              'VPSFREE_DEVCLUSTER_ALLOW_PRECONTRACT_ADOPTION' => '1'
            },
            helper, 'transition-adopt', slug
          )

          refute(result.success?)
          assert_includes(stderr, 'has no recorded socket identity')
          refute(File.exist?(File.join(directory, 'socket-dir')))
          refute(File.exist?(File.join(directory, 'legacy-socket-owner')))
          assert(Process.kill(0, child))
        ensure
          stop_process(child)
        end
      end
    end
  end

  def test_package_transition_refuses_generic_legacy_owner_record
    HELPERS.each do |kind, helper|
      Dir.mktmpdir('devcluster-transition-generic-owner') do |workspace|
        slug = "generic-owner-#{kind}-#{Process.pid}"
        directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
        prefix = kind == 'vpsadmin' ? 'vpsfree-devcluster' : 'vpsadminos-devcluster'
        legacy = "/tmp/#{prefix}-#{Digest::SHA256.hexdigest(slug)[0, 12]}"
        FileUtils.mkdir_p(directory)
        FileUtils.mkdir_p(legacy)
        File.write(File.join(directory, 'socket-dir'), "#{legacy}\n")
        owner = Digest::SHA256.hexdigest("#{workspace}\0#{legacy}")
        File.write(File.join(directory, 'legacy-socket-owner'), "#{owner}\n")

        _stdout, stderr, result = Open3.capture3(
          {
            'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace,
            'VPSFREE_DEVCLUSTER_ALLOW_PRECONTRACT_ADOPTION' => '1'
          },
          helper, 'transition-adopt', slug
        )

        refute(result.success?)
        assert_includes(stderr, 'cluster state cannot be adopted by this package')
        assert_equal(legacy, File.read(File.join(directory, 'socket-dir')).strip)
        assert_equal(owner, File.read(File.join(directory, 'legacy-socket-owner')).strip)
        assert(File.directory?(legacy))
      ensure
        FileUtils.rm_rf(legacy) if legacy
      end
    end
  end

  def test_known_password_reset_legacy_cluster_socket_is_adopted_from_its_runner
    runtime = File.join(ROOT, 'dev-clusters/lib/runtime.sh')
    slug = "legacy-transition-#{Process.pid}"
    prefix = "vpsfree-test-#{Process.pid}"
    Dir.mktmpdir('devcluster-password-reset-transition') do |workspace|
      directory = File.join(workspace, 'cluster')
      FileUtils.mkdir_p(directory)
      legacy = File.join('/tmp', "#{prefix}-#{Digest::SHA256.hexdigest(slug)[0, 12]}")
      child = spawn_marker_process(
        '--sock-dir', legacy,
        '--state-dir', File.join(directory, 'state'),
        '--pid-file', File.join(directory, 'runner.pid'),
        '--ready-file', File.join(directory, 'ready')
      )
      File.write(File.join(directory, 'runner.pid'), "#{child}\n")
      File.write(File.join(directory, 'socket-dir'), "#{legacy}\n")
      begin
        script = <<~'BASH'
          set -euo pipefail
          source "$1"
          cluster_dir() { printf '%s\n' "$CLUSTER_DIR"; }
          pid_file() { printf '%s/runner.pid\n' "$CLUSTER_DIR"; }
          ready_file() { printf '%s/ready\n' "$CLUSTER_DIR"; }
          legacy_socket_transition_allowed() { return 0; }
          devcluster_adopt_package_transition "$SLUG" "$PREFIX"
          devcluster_socket_dir "$SLUG" "$PREFIX"
        BASH
        stdout, stderr, result = Open3.capture3(
          {
            'CLUSTER_DIR' => directory,
            'SLUG' => slug,
            'PREFIX' => prefix,
            'WORKSPACE' => workspace
          },
          'bash', '-c', script, 'bash', runtime
        )
        assert(result.success?, stderr)
        assert_equal(legacy, stdout.strip)
        assert_equal(legacy, File.read(File.join(directory, 'socket-dir')).strip)
        assert(File.file?(File.join(directory, 'legacy-socket-owner')))

        FileUtils.mkdir_p(legacy)
        stop_process(child)
        stdout, stderr, result = Open3.capture3(
          {
            'CLUSTER_DIR' => directory,
            'SLUG' => slug,
            'PREFIX' => prefix,
            'WORKSPACE' => workspace
          },
          'bash', '-c', script, 'bash', runtime
        )
        assert(result.success?, stderr)
        assert_equal(legacy, stdout.strip)
        assert_equal(legacy, File.read(File.join(directory, 'socket-dir')).strip)
      ensure
        stop_process(child)
        FileUtils.rm_rf(legacy)
      end
    end
  end

  def test_legacy_socket_owner_record_cannot_be_reused_by_another_workspace
    runtime = File.join(ROOT, 'dev-clusters/lib/runtime.sh')
    slug = "legacy-owner-#{Process.pid}"
    prefix = "vpsfree-owner-test-#{Process.pid}"
    legacy = File.join('/tmp', "#{prefix}-#{Digest::SHA256.hexdigest(slug)[0, 12]}")
    FileUtils.mkdir_p(legacy)
    Dir.mktmpdir('devcluster-legacy-owner-a') do |workspace_a|
      Dir.mktmpdir('devcluster-legacy-owner-b') do |workspace_b|
        directory_a = File.join(workspace_a, 'cluster')
        directory_b = File.join(workspace_b, 'cluster')
        FileUtils.mkdir_p(directory_a)
        FileUtils.mkdir_p(directory_b)
        child = spawn_marker_process(
          '--sock-dir', legacy,
          '--state-dir', File.join(directory_a, 'state'),
          '--pid-file', File.join(directory_a, 'runner.pid'),
          '--ready-file', File.join(directory_a, 'ready')
        )
        script = <<~'BASH'
          set -euo pipefail
          source "$1"
          cluster_dir() { printf '%s\n' "$CLUSTER_DIR"; }
          pid_file() { printf '%s/runner.pid\n' "$CLUSTER_DIR"; }
          ready_file() { printf '%s/ready\n' "$CLUSTER_DIR"; }
          legacy_socket_transition_allowed() { return 0; }
          die() { printf 'error: %s\n' "$*" >&2; exit 1; }
          devcluster_socket_dir "$SLUG" "$PREFIX"
        BASH
        begin
          File.write(File.join(directory_a, 'runner.pid'), "#{child}\n")
          stdout, stderr, result = Open3.capture3(
            {
              'CLUSTER_DIR' => directory_a,
              'SLUG' => slug,
              'PREFIX' => prefix,
              'WORKSPACE' => workspace_a
            },
            'bash', '-c', script, 'bash', runtime
          )
          assert(result.success?, stderr)
          assert_equal(legacy, stdout.strip)

          FileUtils.cp(File.join(directory_a, 'socket-dir'), directory_b)
          FileUtils.cp(File.join(directory_a, 'legacy-socket-owner'), directory_b)
          File.write(File.join(directory_b, 'runner.pid'), "#{child}\n")
          stdout, stderr, result = Open3.capture3(
            {
              'CLUSTER_DIR' => directory_b,
              'SLUG' => slug,
              'PREFIX' => prefix,
              'WORKSPACE' => workspace_b
            },
            'bash', '-c', script, 'bash', runtime
          )
          refute(result.success?, stdout)
          assert_includes(stderr, 'refusing to replace different cluster socket state')
          assert_equal(legacy, File.read(File.join(directory_b, 'socket-dir')).strip)
          assert_equal(
            File.read(File.join(directory_a, 'legacy-socket-owner')),
            File.read(File.join(directory_b, 'legacy-socket-owner'))
          )
          assert(Process.kill(0, child))
        ensure
          stop_process(child)
        end
      end
    end
  ensure
    FileUtils.rm_rf(legacy) if legacy
  end

  def test_stale_foreign_pid_does_not_adopt_or_kill_a_legacy_cluster
    HELPERS.each do |kind, helper|
      Dir.mktmpdir("devcluster-#{kind}-legacy-owner") do |workspace_a|
        Dir.mktmpdir("devcluster-#{kind}-legacy-stale") do |workspace_b|
          slug = '2026-09-07-shared-legacy-slug'
          directory_a = File.join(workspace_a, '.dev-clusters', kind, 'clusters', slug)
          directory_b = File.join(workspace_b, '.dev-clusters', kind, 'clusters', slug)
          FileUtils.mkdir_p(directory_a)
          FileUtils.mkdir_p(directory_b)
          prefix = kind == 'vpsadmin' ? 'vpsfree' : 'vpsadminos'
          legacy = File.join('/tmp', "#{prefix}-devcluster-#{Digest::SHA256.hexdigest(slug)[0, 12]}")
          child = spawn_marker_process(
            '--sock-dir', legacy,
            '--state-dir', File.join(directory_a, 'state'),
            '--pid-file', File.join(directory_a, 'runner.pid'),
            '--ready-file', File.join(directory_a, 'ready')
          )
          begin
            File.write(File.join(directory_a, 'runner.pid'), "#{child}\n")
            File.write(File.join(directory_b, 'runner.pid'), "#{child}\n")
            _stdout, stderr, result = Open3.capture3(
              {'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace_b}, helper, 'reset', slug
            )
            assert(result.success?, stderr)
            assert(Process.kill(0, child), "#{kind} reset killed a foreign legacy cluster")
            assert(File.directory?(directory_a))
          ensure
            stop_process(child)
          end
        end
      end
    end
  end

  def test_socket_cleanup_fails_if_a_matching_process_survives
    runtime = File.join(ROOT, 'dev-clusters/lib/runtime.sh')
    marker = "/tmp/vpsfree-devcluster-#{Digest::SHA256.hexdigest('signal-test')[0, 12]}"
    child = spawn_marker_process("--socket-path=#{marker}/machine.sock")
    script = <<~BASH
      set -euo pipefail
      source "$1"
      socket_dir() { printf '%s\n' "$EXPECTED_SOCKET_DIR"; }
      die() { printf 'error: %s\n' "$*" >&2; return 1; }
      kill() { return 1; }
      sleep() { :; }
      kill_socket_processes signal-test
    BASH

    begin
      _stdout, stderr, result = Open3.capture3(
        { 'EXPECTED_SOCKET_DIR' => marker }, 'bash', '-c', script, 'bash', runtime
      )
      refute(result.success?)
      assert_includes(stderr, 'unable to stop cluster processes')
      assert(Process.kill(0, child), 'failed cleanup did not leave the test process running')
    ensure
      stop_process(child)
    end
  end

  def test_runner_signal_rechecks_a_process_that_exits
    runtime = File.join(ROOT, 'dev-clusters/lib/runtime.sh')
    marker = "/tmp/vpsfree-devcluster-#{Digest::SHA256.hexdigest('runner-race')[0, 12]}"
    child = spawn_marker_process(marker)
    script = <<~BASH
      set -euo pipefail
      source "$1"
      socket_dir() { printf '%s\n' "$EXPECTED_SOCKET_DIR"; }
      die() { printf 'error: %s\n' "$*" >&2; return 1; }
      kill() {
        builtin kill "$@"
        for _ in {1..1000}; do
          runner_process_matches runner-race "$CHILD_PID" || return 1
          command sleep 0.01
        done
        return 1
      }
      signal_cluster_runner runner-race "$CHILD_PID" TERM stop
    BASH

    begin
      _stdout, stderr, result = Open3.capture3(
        { 'EXPECTED_SOCKET_DIR' => marker, 'CHILD_PID' => child.to_s },
        'bash', '-c', script, 'bash', runtime
      )
      assert(result.success?, stderr)
      assert_process_exited(child, 'runner did not exit after the simulated signal race')
    ensure
      stop_process(child)
    end
  end

  def test_socket_cleanup_rechecks_after_the_final_wait
    runtime = File.join(ROOT, 'dev-clusters/lib/runtime.sh')
    marker = "/tmp/vpsfree-devcluster-#{Digest::SHA256.hexdigest('final-wait')[0, 12]}"
    child = spawn_marker_process("--socket-path=#{marker}/machine.sock")
    script = <<~BASH
      set -euo pipefail
      source "$1"
      WAIT_COUNT=0
      socket_dir() { printf '%s\n' "$EXPECTED_SOCKET_DIR"; }
      die() { printf 'error: %s\n' "$*" >&2; return 1; }
      kill() { return 1; }
      sleep() {
        WAIT_COUNT=$((WAIT_COUNT + 1))
        if [ "$WAIT_COUNT" -eq 21 ]; then
          builtin kill -KILL "$CHILD_PID"
          command sleep 0.2
        fi
      }
      kill_socket_processes final-wait
    BASH

    begin
      _stdout, stderr, result = Open3.capture3(
        { 'EXPECTED_SOCKET_DIR' => marker, 'CHILD_PID' => child.to_s },
        'bash', '-c', script, 'bash', runtime
      )
      assert(result.success?, stderr)
      assert_process_exited(child, 'socket child did not exit during the final wait')
    ensure
      stop_process(child)
    end
  end

  def test_runtime_state_removal_retries_after_the_socket_boundary
    runtime = File.join(ROOT, 'dev-clusters/lib/runtime.sh')
    Dir.mktmpdir('devcluster-removal-retry') do |directory|
      slug = "retry-#{Process.pid}"
      prefix = "vpsfree-test-#{Process.pid}"
      cluster = File.join(directory, 'cluster')
      digest = Digest::SHA256.hexdigest(slug)[0, 12]
      socket = File.join('/tmp', "#{prefix}-#{digest}")
      FileUtils.mkdir_p(cluster)
      FileUtils.mkdir_p(socket)
      File.write(File.join(cluster, 'socket-dir'), "#{socket}\n")
      File.write(File.join(socket, 'machine.sock'), "socket\n")
      interrupted = <<~'BASH'
        set -euo pipefail
        source "$1"
        cluster_dir() { printf '%s\n' "$CLUSTER_DIR"; }
        pid_file() { printf '%s/runner.pid\n' "$CLUSTER_DIR"; }
        legacy_socket_transition_allowed() { return 0; }
        record_legacy_socket_owner retry "$SOCKET_DIR"
        REMOVE_COUNT=0
        rm() {
          REMOVE_COUNT=$((REMOVE_COUNT + 1))
          if [ "$REMOVE_COUNT" -eq 1 ]; then
            command rm "$@"
            return 0
          fi
          return 1
        }
        remove_cluster_runtime_state retry "$SOCKET_DIR"
      BASH
      _stdout, _stderr, result = Open3.capture3(
        {'CLUSTER_DIR' => cluster, 'SOCKET_DIR' => socket, 'WORKSPACE' => directory},
        'bash', '-c', interrupted, 'bash', runtime
      )
      refute(result.success?)
      refute(File.exist?(socket))
      assert(File.directory?(cluster))

      retry_script = <<~'BASH'
        set -euo pipefail
        source "$1"
        cluster_dir() { printf '%s\n' "$CLUSTER_DIR"; }
        pid_file() { printf '%s/runner.pid\n' "$CLUSTER_DIR"; }
        legacy_socket_transition_allowed() { return 0; }
        RESOLVED_SOCKET_DIR="$(devcluster_socket_dir "$SLUG" "$PREFIX")"
        [ "$RESOLVED_SOCKET_DIR" = "$SOCKET_DIR" ]
        remove_cluster_runtime_state "$SLUG" "$RESOLVED_SOCKET_DIR"
      BASH
      _stdout, stderr, result = Open3.capture3(
        {
          'CLUSTER_DIR' => cluster,
          'SOCKET_DIR' => socket,
          'SLUG' => slug,
          'PREFIX' => prefix,
          'WORKSPACE' => directory
        },
        'bash', '-c', retry_script, 'bash', runtime
      )
      assert(result.success?, stderr)
      refute(File.exist?(socket))
      refute(File.exist?(cluster))
    ensure
      FileUtils.rm_rf(socket) if socket
    end
  end

  private

  def runtime_contract
    @runtime_contract ||= JSON.parse(
      File.read(File.join(ROOT, 'portal/internal/session/runtime-contract.json'))
    )
  end

  def prepare_lifecycle_reset(workspace, kind, slug)
    write_lifecycle(workspace, slug, 'active')
    lock_root = File.join(workspace, 'worktrees', '.locks')
    directory = File.join(workspace, '.dev-clusters', kind, 'clusters', slug)
    FileUtils.mkdir_p(lock_root)
    FileUtils.mkdir_p(directory)
    File.write(File.join(directory, 'sentinel'), "keep\n")
    journal = File.join(lock_root, "#{slug}.archive.json")
    File.write(journal, "{}\n")
    File.chmod(0o600, journal)
    lock_path = File.join(lock_root, "#{slug}.lock")
    File.open(lock_path, File::WRONLY | File::CREAT, 0o600) {}
    File.chmod(0o600, lock_path)
    [directory, lock_path]
  end

  def run_lifecycle_reset(helper, workspace, slug, lock, lock_path, extra_environment = {})
    Open3.capture3(
      {
        'VPSFREE_DEVCLUSTER_WORKSPACE' => workspace,
        'VPSFREE_DEV_SESSION_LIFECYCLE_OPERATION' => 'archive',
        'VPSFREE_DEV_SESSION_LIFECYCLE_LOCK_FD' => lock.fileno.to_s,
        'VPSFREE_DEV_SESSION_LIFECYCLE_LOCK_PATH' => lock_path
      }.merge(extra_environment),
      helper, 'reset', slug,
      lock.fileno => lock.fileno
    )
  end

  def workspace_socket_directory(kind, workspace, slug)
    prefix = kind == 'vpsadmin' ? 'vpsfree' : 'vpsadminos'
    digest = Digest::SHA256.hexdigest("#{File.realpath(workspace)}\0#{slug}")[0, 12]
    File.join('/tmp', "#{prefix}-devcluster-#{digest}")
  end

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

  def spawn_marker_process(*arguments)
    pid = Process.spawn(
      RbConfig.ruby, '-e', 'sleep 30', '--', *arguments,
      out: File::NULL, err: File::NULL
    )
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 2
    loop do
      cmdline = File.read("/proc/#{pid}/cmdline").split("\0")
      return pid if arguments.all? { |argument| cmdline.include?(argument) }
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

  def assert_process_exited(pid, message)
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 2
    loop do
      return if Process.wait(pid, Process::WNOHANG)
      raise message if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline

      sleep 0.01
    end
  rescue Errno::ECHILD
    nil
  end
end
