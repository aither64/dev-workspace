# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_archive_permissions_remove_readonly_tracking_and_checkout_caches
    skip 'requires non-root permission checks' if Process.euid.zero?
    with_archive_cleanup_fixture do |runner, workspace, slug, common, checkout|
      head = git_capture_success('git', '-C', checkout, 'rev-parse', 'HEAD').strip
      tracking = File.join(workspace, 'work', slug)
      outside = File.join(workspace, 'outside')
      FileUtils.mkdir_p(outside)
      File.write(File.join(outside, 'keep'), 'outside')
      File.chmod(0o500, outside)
      [tracking, checkout].each do |root|
        cache = File.join(root, 'module-cache', 'nested')
        FileUtils.mkdir_p(cache)
        File.write(File.join(cache, 'raw.json'), '{}')
        File.write(File.join(cache, 'README.md'), 'retained explanation')
        File.symlink(outside, File.join(cache, 'outside'))
        File.chmod(0o400, File.join(cache, 'raw.json'), File.join(cache, 'README.md'))
        File.chmod(0o500, cache)
        File.chmod(0o555, File.dirname(cache))
      end
      group = File.dirname(checkout)
      File.chmod(0o500, group)
      result = runner.archive_check(slug, as_is: true)
      assert(result.fetch('ready'), result.fetch('blockers').inspect)
      assert_equal(0o500, File.stat(group).mode & 0o777)
      assert_equal(0o500, File.stat(File.join(tracking, 'module-cache', 'nested')).mode & 0o777)

      runner.archive(slug, as_is: true, explicit: true)
      refute(File.exist?(checkout))
      retained = File.join(workspace, 'archive', slug, 'module-cache', 'nested')
      assert_equal('retained explanation', File.read(File.join(retained, 'README.md')))
      assert_equal(0o400, File.stat(File.join(retained, 'README.md')).mode & 0o777)
      assert_equal(0o500, File.stat(retained).mode & 0o777)
      refute(File.exist?(File.join(retained, 'raw.json')))
      assert_equal('outside', File.read(File.join(outside, 'keep')))
      assert_equal(0o500, File.stat(outside).mode & 0o777)
      assert_equal(head, git_capture_success('git', "--git-dir=#{common}", 'rev-parse', "refs/heads/#{slug}").strip)
      runner.revive(slug, as_is: true)
      assert_equal(head, git_capture_success('git', '-C', checkout, 'rev-parse', 'HEAD').strip)
      refute(File.exist?(File.join(checkout, 'module-cache')))
    ensure
      restore_archive_test_directory_access(workspace)
    end
  end

  def test_archive_permissions_pruning_dry_run_and_failure_preserve_modes
    Dir.mktmpdir do |root|
      cache = File.join(root, 'cache')
      FileUtils.mkdir_p(cache)
      File.write(File.join(cache, 'raw.json'), '{}')
      File.write(File.join(root, 'portal.yml'), 'invalid: [')
      File.chmod(0o500, root, cache)
      assert_equal([File.join(cache, 'raw.json')], WorkspaceTrackingSummaries.prune(root))
      assert_equal(0o500, File.stat(cache).mode & 0o777)
      assert(File.exist?(File.join(cache, 'raw.json')))
      assert_raises(Psych::SyntaxError) { WorkspaceTrackingSummaries.prune(root, apply: true) }
      assert_equal(0o500, File.stat(root).mode & 0o777)
    ensure
      File.chmod(0o700, root) if File.directory?(root)
    end
  end

  def test_archive_permissions_preflight_rejects_inaccessible_tracking_and_shared_parents
    skip 'requires non-root permission checks' if Process.euid.zero?
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, checkout|
      cache = File.join(workspace, 'work', slug, 'cache')
      FileUtils.mkdir_p(cache)
      File.chmod(0o400, cache)
      result = runner.archive_check(slug, as_is: true)
      refute(result.fetch('ready'))
      assert(result.fetch('blockers').any? { |item| item['code'] == 'removal_permission' && item['path'] == cache })
      File.chmod(0o700, cache)
      parent = File.dirname(File.dirname(checkout))
      File.chmod(0o500, parent)
      result = runner.archive_check(slug, as_is: true)
      refute(result.fetch('ready'))
      assert(result.fetch('blockers').any? { |item| item['code'] == 'removal_parent_permission' && item['path'] == parent })
      assert_equal(0o500, File.stat(parent).mode & 0o777)
    ensure
      File.chmod(0o700, cache) if cache && File.directory?(cache)
      File.chmod(0o700, parent) if parent && File.directory?(parent)
    end
  end

  def test_archive_permissions_restore_after_removal_failure_and_retry
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, checkout|
      cache = File.join(checkout, 'cache')
      FileUtils.mkdir_p(cache)
      File.write(File.join(cache, 'raw'), 'disposable')
      File.chmod(0o500, cache)
      runner.define_singleton_method(:archive_cleanup_remove!) { |*, **| raise Errno::EACCES, cache }
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
      assert_includes(error.message, cache)
      assert_includes(error.message, 'Permission denied')
      assert_equal(0o500, File.stat(cache).mode & 0o777)
      runner.singleton_class.remove_method(:archive_cleanup_remove!)
      runner.archive(slug, as_is: true, explicit: true)
      refute(File.exist?(checkout))
    end
  end

  def test_archive_permissions_report_chmod_failure_and_restore_previous_changes
    skip 'requires non-root permission checks' if Process.euid.zero?
    Dir.mktmpdir do |root|
      paths = %w[first second].map { |name| File.join(root, name) }
      paths.each { |path| Dir.mkdir(path, 0o500) }
      chmod = File.method(:chmod)
      failure = lambda do |mode, path|
        raise Errno::EROFS, path if path == paths.last && mode == 0o700
        chmod.call(mode, path)
      end
      File.define_singleton_method(:chmod, failure)
      begin
        error = assert_raises(WorkspaceCleanupPermissions::Error) do
          WorkspaceCleanupPermissions.with_writable_directories(paths) { flunk('deletion proceeded after chmod failed') }
        end
        assert_includes(error.message, paths.last)
        assert_includes(error.message, 'Read-only file system')
      ensure
        File.define_singleton_method(:chmod, chmod)
      end
      paths.each { |path| assert_equal(0o500, File.stat(path).mode & 0o777) }
    end
  end

  def test_archive_permissions_never_chmod_unowned_directories
    skip 'requires non-root permission checks' if Process.euid.zero?
    Dir.mktmpdir do |root|
      File.chmod(0o500, root)
      stat = File.lstat(root)
      stat.define_singleton_method(:owned?) { false }
      lstat = File.method(:lstat)
      File.define_singleton_method(:lstat) { |_| stat }
      begin
        refute(WorkspaceCleanupPermissions.removable_directory?(root))
        assert_raises(WorkspaceCleanupPermissions::Error) do
          WorkspaceCleanupPermissions.with_writable_directories([root]) { flunk('unowned directory was made writable') }
        end
      ensure
        File.define_singleton_method(:lstat, lstat)
      end
      assert_equal(0o500, File.stat(root).mode & 0o777)
    ensure
      File.chmod(0o700, root)
    end
  end

  def test_archive_permissions_legacy_retry_restores_only_directory_write_additions
    %w[git files].each do |kind|
      with_archive_cleanup_fixture do |runner, workspace, slug, _common, checkout|
        enable_legacy_archive_capture(runner)
        cache = File.join(checkout, 'cache')
        FileUtils.mkdir_p(cache)
        File.write(File.join(cache, 'raw'), 'saved') if kind == 'files'
        File.chmod(0o500, cache)
        child = fork do
          runner.define_singleton_method(:archive_cleanup_remove!) { |*, **| Process.exit!(73) }
          runner.archive(slug, as_is: true, explicit: true)
          Process.exit!(0)
        end
        _, status = Process.wait2(child)
        assert_equal(73, status.exitstatus)
        assert_equal(0o700, File.stat(cache).mode & 0o777)

        File.chmod(0o750, cache)
        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
        assert_includes(error.message, 'worktree changed after archive recovery capture')
        assert_equal(0o750, File.stat(cache).mode & 0o777)
        File.chmod(0o700, cache)
        if kind == 'files'
          File.write(File.join(cache, 'raw'), 'changed')
          error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
          assert_includes(error.message, 'worktree changed after archive recovery capture')
          File.write(File.join(cache, 'raw'), 'saved')
        end
        runner.archive(slug, as_is: true, explicit: true)
        runner.revive(slug, as_is: true)
        assert_equal(0o500, File.stat(cache).mode & 0o777)
        assert_equal('saved', File.read(File.join(cache, 'raw'))) if kind == 'files'
      ensure
        restore_archive_test_directory_access(workspace)
      end
    end
  end

  private

  def restore_archive_test_directory_access(workspace)
    WorkspaceCleanupPermissions.each_directory(workspace) do |path|
      File.chmod((File.stat(path).mode & 0o7777) | 0o700, path)
    end
  end
end
