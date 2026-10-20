# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_archive_retry_accepts_reused_git_admin_name_without_touching_live_worktree
    with_archive_cleanup_fixture do |runner, workspace, slug, common, checkout|
      runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
      admin = File.realpath(git_capture_success('git', '-C', checkout, 'rev-parse', '--absolute-git-dir').strip)
      # Keep the unlinked original inode allocated so Git's replacement has a
      # distinct identity even on filesystems that immediately recycle inodes.
      File.open(admin, File::RDONLY) do
        runner.define_singleton_method(:retire_session_runtime!) { |*| raise DevSession::Error, 'pause after removal' }
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        journal = runner.send(:load_archive_journal, slug)
        assert_equal(4, journal.fetch('schema'))
        refute(File.exist?(checkout))
        live = File.join(workspace, 'worktrees', 'other-session', 'sample')
        assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', live, 'master')
        assert_equal(admin, git_capture_success('git', '-C', live, 'rev-parse', '--absolute-git-dir').strip)
        File.binwrite(File.join(live, 'unsaved'), "live bytes\x00\xff".b)
        before = [File.binread(File.join(live, '.git')), File.binread(File.join(live, 'unsaved')),
                  git_capture_success('git', '-C', live, 'status', '--porcelain'),
                  git_capture_success('git', '-C', live, 'rev-parse', 'HEAD'), File.stat(admin).ino]
        runner.with_verified_removed_archive(slug, journal) { assert(true) }
        runner.singleton_class.remove_method(:retire_session_runtime!)
        cleanup = DevSession::ArchiveCleanup.new(runner, slug)
        cleanup.define_singleton_method(:discard_completed!) { |*, **| raise DevSession::Error, 'pause after journal removal' }
        with_archive_cleanup_instance(cleanup) do
          error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
          assert_includes(error.message, 'pause after journal removal')
        end
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
        completed = cleanup.completed_journal(cleanup.load)
        runner.with_verified_removed_archive(slug, completed) { assert(true) }
        cleanup.singleton_class.remove_method(:discard_completed!)
        runner.archive(slug, as_is: true)
        refute(File.exist?(cleanup.path))
        assert_equal(before, [File.binread(File.join(live, '.git')), File.binread(File.join(live, 'unsaved')),
                              git_capture_success('git', '-C', live, 'status', '--porcelain'),
                              git_capture_success('git', '-C', live, 'rev-parse', 'HEAD'), File.stat(admin).ino])
      end
    end
  end

  def test_removed_archive_recovery_refuses_remaining_checkout_or_unverified_replacement
    with_archive_cleanup_fixture do |runner, workspace, slug, common, checkout|
      runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
      admin = git_capture_success('git', '-C', checkout, 'rev-parse', '--absolute-git-dir').strip
      File.open(admin, File::RDONLY) do
        runner.define_singleton_method(:retire_session_runtime!) { |*| raise DevSession::Error, 'pause after removal' }
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        journal = runner.send(:load_archive_journal, slug)
        live = File.join(workspace, 'worktrees', 'other-session', 'sample')
        assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', live, 'master')
        pointer = File.binread(File.join(live, '.git'))
        File.write(File.join(live, '.git'), 'broken pointer')
        assert_raises(DevSession::Error) { runner.with_verified_removed_archive(slug, journal) { flunk('invalid replacement accepted') } }
        File.binwrite(File.join(live, '.git'), pointer)
        cleanup = DevSession::ArchiveCleanup.new(runner, slug)
        sidecar = cleanup.load(journal:)
        sidecar.fetch('inventory').fetch('worktrees').first['removed'] = false
        # Progress is excluded from the sealed inventory digest.
        cleanup.send(:write, sidecar)
        error = assert_raises(DevSession::Error) { runner.with_verified_removed_archive(slug, journal) { flunk('unfinished removal accepted') } }
        assert_includes(error.message, 'recorded checkout removal')
        assert_equal(journal, runner.send(:load_archive_journal, slug))
        assert_equal(pointer, File.binread(File.join(live, '.git')))
      end
    end
  end

  def test_explicit_local_archive_without_tracking_history_retries_sync_and_revives
    [false, true].product([false, true]).each do |partial, sync_before_revive|
      with_workspace do |workspace|
        slug = '2026-06-06-no-tracking-history'
        runner = automatic_fixture(workspace, slug, tracking_commit: false)
        runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
        if partial
          assert_git_success('git', '-C', workspace, 'add', File.join('work', slug, 'plan.md'))
          assert_git_success('git', '-C', workspace, 'commit', '-m', 'plan only')
          assert_git_success('git', '-C', workspace, 'push', 'origin', 'master')
        end
        plan = File.read(File.join(workspace, 'work', slug, 'plan.md'))
        File.write(File.join(workspace, 'unrelated'), 'keep this staged')
        assert_git_success('git', '-C', workspace, 'add', 'unrelated')
        staged = git_capture_success('git', '-C', workspace, 'diff', '--cached', '--binary')
        runner.define_singleton_method(:finalize_locked!) { |*, **| raise DevSession::Error, 'interrupted before move' }
        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
        assert_includes(error.message, 'interrupted before move')
        runner.singleton_class.remove_method(:finalize_locked!)
        runner.archive(slug, as_is: true, explicit: true)
        assert_equal(plan, File.read(File.join(workspace, 'archive', slug, 'plan.md')))
        assert_equal('pending', runner.tracking_sync_receipt(slug).fetch('state'))
        runner.define_singleton_method(:require_linear_workspace_master_for_tracking!) { |**| raise DevSession::Error, 'remote unavailable' }
        runner.tracking_sync(slug, force: true)
        assert_includes(runner.tracking_sync_receipt(slug).fetch('error'), 'remote unavailable')
        runner.singleton_class.remove_method(:require_linear_workspace_master_for_tracking!)
        if sync_before_revive
          runner.tracking_sync(slug, force: true)
          assert_equal('synced', runner.tracking_sync_receipt(slug).fetch('state'))
        end
        runner.define_singleton_method(:start) { |*, **| nil }
        runner.revive(slug, as_is: true)
        assert_equal(plan, File.read(File.join(workspace, 'work', slug, 'plan.md')))
        runner.tracking_sync(slug, force: true)
        assert_equal('synced', runner.tracking_sync_receipt(slug).fetch('state'))
        assert_equal(staged, git_capture_success('git', '-C', workspace, 'diff', '--cached', '--binary'))
      end
    end
  end

  def test_git_archive_retains_checkout_when_saved_blobs_are_missing_or_corrupt
    require 'zlib'
    %w[missing corrupt].product(%w[before_capture before_removal]).each do |damage, timing|
      with_archive_cleanup_fixture do |runner, _workspace, slug, common, checkout|
        object = git_capture_success('git', '-C', checkout, 'rev-parse', 'HEAD:README.md').strip
        path = File.join(common, 'objects', object[0, 2], object[2..])
        damage_blob = lambda do
          if damage == 'missing'
            File.unlink(path)
          else
            File.chmod(0o600, path)
            File.binwrite(path, Zlib::Deflate.deflate("blob 6\0wrong\n"))
          end
        end
        assert_empty(git_capture_success('git', '-C', checkout, 'status', '--porcelain').strip)
        if timing == 'before_capture'
          damage_blob.call
        else
          runner.define_singleton_method(:archive_cleanup_recovery) do |*arguments|
            recovery = super(*arguments)
            unless recovery.instance_variable_defined?(:@fault_fixture_installed)
              recovery.instance_variable_set(:@fault_fixture_installed, true)
              calls = 0
              recovery.define_singleton_method(:verify_checkout!) do |record|
                calls += 1
                damage_blob.call if calls == 2
                super(record)
              end
            end
            recovery
          end
        end
        assert_raises(DevSession::Error, DevSession::CommandError) { runner.archive(slug, as_is: true, explicit: true) }
        assert_equal("# Test\n", File.read(File.join(checkout, 'README.md')), "#{damage} #{timing} removed original bytes")
      end
    end
  end

  def test_git_archive_restore_repopulates_interrupted_checkout_without_filters
    with_archive_cleanup_fixture do |runner, workspace, slug, common, checkout|
      File.write(File.join(checkout, '.gitattributes'), "README.md filter=changed\n")
      assert_git_success('git', '-C', checkout, 'add', '.gitattributes')
      assert_git_success('git', '-C', checkout, 'commit', '-m', 'raw clean fixture')
      before = git_capture_success('git', '-C', checkout, 'ls-files', '--stage')
      runner.archive(slug, as_is: true, explicit: true)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      recovery = runner.archive_cleanup_recovery(slug, manifest.fetch('archive_operation_id'))
      record = recovery.records.first
      saved, destination = recovery.send(:read_record, record)
      assert_equal('git', saved.fetch('kind'))
      recovery.send(:write_json, File.join(destination, 'restoring.json'), record.slice('path', 'common_dir', 'head', 'branch'))
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--no-checkout', checkout, record.fetch('branch'))
      File.write(File.join(checkout, 'README.md'), 'partial wrong bytes')
      assert_git_success('git', "--git-dir=#{common}", 'config', 'filter.changed.smudge', 'false')
      recovery.restore!(record)
      assert_equal("# Test\n", File.read(File.join(checkout, 'README.md')))
      assert_equal("README.md filter=changed\n", File.read(File.join(checkout, '.gitattributes')))
      assert_equal(before, git_capture_success('git', '-C', checkout, 'ls-files', '--stage'))
    end
  end

  def test_tracking_sync_hanging_hook_releases_lifecycle_locks_and_preserves_shared_index
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, _checkout|
      runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
      runner.archive(slug, as_is: true, explicit: true)
      File.write(File.join(workspace, 'unrelated'), 'staged outside session')
      assert_git_success('git', '-C', workspace, 'add', 'unrelated')
      staged = git_capture_success('git', '-C', workspace, 'diff', '--cached', '--binary', '--', 'unrelated')
      started = File.join(workspace, '.git', 'hook-started')
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, "#!/bin/sh\n: > #{started.shellescape}\nwhile :; do sleep 1; done\n")
      File.chmod(0o700, hook)
      runner.define_singleton_method(:tracking_sync_attempt_seconds) { 5 }
      sync = Thread.new { runner.tracking_sync(slug, force: true) }
      deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 10
      until File.file?(started)
        if !sync.alive? || Process.clock_gettime(Process::CLOCK_MONOTONIC) > deadline
          flunk "tracking hook did not start: #{runner.tracking_sync_receipt(slug).inspect}; alive=#{sync.alive?}"
        end
        sleep 0.02
      end
      reviver = archive_runner_for(workspace, env: {'DEV_WORKSPACE_DEFER_TRACKING' => '1'})
      revive = Thread.new { reviver.revive(slug, as_is: true) }
      assert(sync.join(10), 'tracking hook retained lifecycle locks')
      assert(revive.join(10), 'revival did not continue after tracking sync yielded access')
      revive.value
      File.unlink(hook)
      assert_equal('revive', reviver.tracking_sync_receipt(slug).fetch('direction'))
      reviver.tracking_sync(slug, force: true)
      assert_equal('synced', reviver.tracking_sync_receipt(slug).fetch('state'))
      assert_equal(staged, git_capture_success('git', '-C', workspace, 'diff', '--cached', '--binary', '--', 'unrelated'))
    ensure
      File.unlink(hook) if hook && File.file?(hook)
      sync&.join(10)
      revive&.join(10)
    end
  end

  def test_supervised_archive_authorization_requires_the_exact_portal_receipt
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-worker'
      operation = 'a' * 64
      receipt_id = 'b' * 64
      unit = "workspace-archive-#{receipt_id}-1.service"
      identity = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      path = File.join(runner.instance_variable_get(:@workspace_state_root), 'portal', identity, 'lifecycle-operations.json')
      FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
      File.write(path, JSON.generate('schema' => 2, 'workspace' => workspace, 'operations' => [{
        'slug' => slug, 'kind' => 'archive', 'state' => 'running', 'receiptId' => receipt_id,
        'attempt' => 1, 'options' => { 'journalId' => operation }
      }]))
      File.chmod(0o600, path)
      assert(runner.authorized_archive_worker?(slug, operation, unit))
      refute(runner.authorized_archive_worker?(slug, 'c' * 64, unit))
      refute(runner.authorized_archive_worker?(slug, operation, unit.sub('-1.service', '-2.service')))
      refute(runner.authorized_archive_worker?('other-session', operation, unit))
    end
  end

  def test_local_archive_and_revive_coalesce_pending_tracking_without_fetching
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, _feature|
      runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
      head = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip
      runner.define_singleton_method(:require_linear_workspace_master_for_tracking!) do |**|
        raise DevSession::Error, 'remote unavailable'
      end

      runner.archive(slug, as_is: true, explicit: true)
      receipt = runner.tracking_sync_receipt(slug)
      assert_equal('pending', receipt.fetch('state'))
      assert_equal('archive', receipt.fetch('direction'))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal(head, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip)
      runner.tracking_sync(slug, force: true)
      assert_includes(runner.tracking_sync_receipt(slug).fetch('error'), 'remote unavailable')

      runner.revive(slug, as_is: true)
      receipt = runner.tracking_sync_receipt(slug)
      assert_equal('pending', receipt.fetch('state'))
      assert_equal('revive', receipt.fetch('direction'))
      assert_equal(head, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip)
      refute(File.exist?(File.join(workspace, 'archive', slug)))

      runner.singleton_class.remove_method(:require_linear_workspace_master_for_tracking!)
      runner.tracking_sync(slug, force: true)
      assert_equal('synced', runner.tracking_sync_receipt(slug).fetch('state'))
      assert(File.directory?(File.join(workspace, 'work', slug)))
      assert_git_success('git', '-C', workspace, 'diff', '--quiet', 'HEAD', '--', File.join('work', slug))
    end
  end

  def test_clean_archive_uses_git_recovery_and_preserves_permissions_and_empty_directories
    with_archive_cleanup_fixture do |runner, workspace, slug, common, _feature|
      checkout = File.join(workspace, 'worktrees', slug, 'sample')
      file = Dir.glob(File.join(checkout, '*')).find { |path| File.file?(path) }
      File.chmod(0o640, file)
      FileUtils.mkdir_p(File.join(checkout, 'empty'))
      File.chmod(0o750, File.join(checkout, 'empty'))
      runner.archive(slug, as_is: true, explicit: true)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      recovery = runner.send(:archive_cleanup_recovery, slug, manifest.fetch('archive_operation_id'))
      record = recovery.records.find { |entry| entry.fetch('path') == 'sample' }
      saved, destination = recovery.send(:read_record, record)
      assert_equal('git', saved.fetch('kind'))
      assert_empty(Dir.children(File.join(destination, 'files')))
      assert_empty(git_capture_success('git', "--git-dir=#{common}", 'for-each-ref', '--format=%(refname)',
                                      "refs/dev-session-archives/#{manifest.fetch('archive_operation_id')}/#{Digest::SHA256.hexdigest('sample')}/index").strip)
      runner.revive(slug, as_is: true)
      assert_equal(0o640, File.stat(file).mode & 0o777)
      assert_equal(0o750, File.stat(File.join(checkout, 'empty')).mode & 0o777)
    end
  end

  def test_tracking_digest_cache_detects_same_size_rewrite_with_restored_mtime
    with_workspace do |workspace|
      runner = runner_for(workspace)
      root = File.join(workspace, 'records')
      FileUtils.mkdir_p(root)
      path = File.join(root, 'record')
      File.write(path, 'first')
      before = File.stat(path)
      digest = runner.send(:tracking_tree_sha256, root)
      File.write(path, 'other')
      File.utime(before.atime, before.mtime, path)
      refute_equal(digest, runner.send(:tracking_tree_sha256, root))
    end
  end

  def test_explicit_archive_retains_conflicted_index_and_working_contents
    with_archive_cleanup_fixture do |runner, _workspace, slug, _common, checkout|
      objects = %w[base ours theirs].map do |content|
        stdout, stderr, status = Open3.capture3('git', '-C', checkout, 'hash-object', '-w', '--stdin', stdin_data: content)
        assert(status.success?, stderr)
        stdout.strip
      end
      entries = "0 #{'0' * 40}\tconflict\n" + objects.each_with_index.map do |object, index|
        "100644 #{object} #{index + 1}\tconflict\n"
      end.join
      _out, err, status = Open3.capture3('git', '-C', checkout, 'update-index', '--index-info', stdin_data: entries)
      assert(status.success?, err)
      File.binwrite(File.join(checkout, 'conflict'), "working\x00resolution".b)
      before = git_capture_success('git', '-C', checkout, 'ls-files', '--stage')
      runner.archive(slug, as_is: true, explicit: true)
      runner.revive(slug, as_is: true)
      assert_equal(before, git_capture_success('git', '-C', checkout, 'ls-files', '--stage'))
      assert_equal("working\x00resolution".b, File.binread(File.join(checkout, 'conflict')))
    end
  end

  def test_git_clean_filtered_content_keeps_a_full_recovery_snapshot
    with_archive_cleanup_fixture do |runner, workspace, slug, common, checkout|
      clean = [RbConfig.ruby, '-e', "STDOUT.write(STDIN.read.gsub('working', 'stored'))"].shelljoin
      smudge = [RbConfig.ruby, '-e', "STDOUT.write(STDIN.read.gsub('stored', 'working'))"].shelljoin
      assert_git_success('git', '-C', checkout, 'config', 'filter.example.clean', clean)
      assert_git_success('git', '-C', checkout, 'config', 'filter.example.smudge', smudge)
      File.write(File.join(checkout, '.gitattributes'), "filtered filter=example\n")
      File.write(File.join(checkout, 'filtered'), 'working contents')
      assert_git_success('git', '-C', checkout, 'add', '.gitattributes', 'filtered')
      assert_git_success('git', '-C', checkout, 'commit', '-m', 'filtered fixture')
      assert_empty(git_capture_success('git', '-C', checkout, 'status', '--porcelain').strip)
      runner.archive(slug, as_is: true, explicit: true)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      recovery = runner.archive_cleanup_recovery(slug, manifest.fetch('archive_operation_id'))
      saved, = recovery.send(:read_record, recovery.records.first)
      assert_equal('files', saved.fetch('kind'))
      assert_git_success('git', "--git-dir=#{common}", 'config', '--remove-section', 'filter.example')
      runner.revive(slug, as_is: true)
      assert_equal('working contents', File.read(File.join(checkout, 'filtered')))
    end
  end

  def test_archive_private_records_under_restrictive_umask
    %w[active complete].each do |lifecycle|
      with_archive_cleanup_fixture do |runner, workspace, slug, _common, _feature|
        if lifecycle == 'complete'
          set_lifecycle(workspace, slug, lifecycle)
          assert_git_success('git', '-C', workspace, 'add', File.join('work', slug, 'state.md'))
          assert_git_success('git', '-C', workspace, 'commit', '-m', 'complete fixture')
        end
        tracking = File.join(workspace, 'work', slug)
        Dir[File.join(tracking, '*')].each { |path| File.chmod(0o600, path) if File.file?(path) }
        previous_umask = File.umask(0o077)
        begin
          runner.archive(slug, as_is: true)
        ensure
          File.umask(previous_umask)
        end
        assert(File.directory?(File.join(workspace, 'archive', slug)))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
        refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
      end
    end
  end

  def test_archive_retry_accepts_permission_changes_but_rejects_changed_records
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, _feature|
      runner.define_singleton_method(:commit_tracking_transition!) { |*, **| raise DevSession::Error, 'pause before commit' }
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      runner.singleton_class.remove_method(:commit_tracking_transition!)
      tracking = File.join(workspace, 'archive', slug)
      path = File.join(tracking, 'plan.md')
      content = File.binread(path)
      journal = runner.send(:lifecycle_journal_file, slug, 'archive')
      original_journal = File.binread(journal)
      %i[contents path type].each do |change|
        case change
        when :contents then File.write(path, content + 'changed')
        when :path then File.rename(path, path + '.renamed')
        when :type
          File.unlink(path)
          File.symlink('state.md', path)
        end
        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        assert_includes(error.message, 'archived tracking changed')
        assert_equal(original_journal, File.binread(journal))
        File.rename(path + '.renamed', path) if change == :path
        File.unlink(path) if change == :type
        File.binwrite(path, content)
      end
      Dir[File.join(tracking, '*')].each { |file| File.chmod(0o600, file) if File.file?(file) }
      File.chmod(0o700, tracking)
      runner.archive(slug, as_is: true)
      refute(File.exist?(journal))
    end
  end

  def test_tracking_content_digest_preserves_symlink_targets
    with_workspace do |workspace|
      runner = runner_for(workspace)
      root = File.join(workspace, 'records')
      FileUtils.mkdir_p(root)
      File.symlink('first', File.join(root, 'link'))
      digest = runner.send(:tracking_tree_sha256, root)
      File.unlink(File.join(root, 'link'))
      File.symlink('second', File.join(root, 'link'))
      refute(runner.send(:tracking_tree_matches?, root, digest))
    end
  end

  def test_archive_retries_legacy_prepared_intent_and_moved_tracking
    %i[prepared moved].each do |phase|
      with_archive_cleanup_fixture do |runner, workspace, slug, _common, _feature|
        runner.define_singleton_method(:tracking_tree_sha256) do |root, **keywords|
          super(root, **keywords, legacy_permissions: true)
        end
        if phase == :prepared
          runner.define_singleton_method(:write_lifecycle_journal) { |*, **| raise DevSession::Error, 'pause after sidecar' }
        else
          runner.define_singleton_method(:commit_tracking_transition!) { |*, **| raise DevSession::Error, 'pause after move' }
        end
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        runner.singleton_class.remove_method(:tracking_tree_sha256)
        runner.singleton_class.remove_method(phase == :prepared ? :write_lifecycle_journal : :commit_tracking_transition!)
        runner.archive(slug, as_is: true)
        assert(File.directory?(File.join(workspace, 'archive', slug)))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
        refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
      end
    end
  end

  def test_archive_shared_master_preserves_the_sealed_head_after_its_tracking_commit
    with_shared_master_archive_fixture do |runner, workspace, slug, sealed|
      runner.archive(slug, as_is: true)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(sealed, manifest.fetch('repositories').fetch(0).fetch('final_head_sha'))
      refute_equal(sealed, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip)
      assert_equal(sealed, git_capture_success('git', '-C', workspace, 'rev-parse', 'refs/remotes/origin/master').strip)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
    end
  end

  def test_archive_shared_master_retry_accepts_another_tracking_commit_without_retargeting
    with_shared_master_archive_fixture do |runner, workspace, slug, sealed|
      runner.define_singleton_method(:commit_tracking_transition!) do |*_args, **_options|
        raise DevSession::Error, 'pause before archive tracking commit'
      end
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      assert_includes(error.message, 'pause before archive tracking commit')
      journal_path = runner.send(:lifecycle_journal_file, slug, 'archive')
      journal = JSON.parse(File.read(journal_path))
      assert_equal('tracking_archived', journal.fetch('phase'))
      assert_equal({ 'workspace' => sealed }, journal.fetch('proven_heads'))
      original_sidecar = DevSession::ArchiveCleanup.new(runner, slug).load
      runner.singleton_class.remove_method(:commit_tracking_transition!)

      other = File.join(workspace, 'work', '2026-06-07-other', 'state.md')
      FileUtils.mkdir_p(File.dirname(other))
      File.write(other, "---\nlifecycle: active\n---\n\nOther tracking checkpoint.\n")
      assert_git_success('git', '-C', workspace, 'add', other)
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'record other tracking checkpoint')
      assert_git_success('git', '-C', workspace, 'push', 'origin', 'master')
      assert_equal(original_sidecar, DevSession::ArchiveCleanup.new(runner, slug).load)
      assert_equal(journal, JSON.parse(File.read(journal_path)))

      runner.archive(slug, as_is: true)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(sealed, manifest.fetch('repositories').fetch(0).fetch('final_head_sha'))
      assert_equal('2', git_capture_success('git', '-C', workspace, 'rev-list', '--count', "#{sealed}..HEAD").strip)
      assert_equal(File.binread(other), git_capture_success('git', '-C', workspace, 'show', 'HEAD:work/2026-06-07-other/state.md'))
      refute(File.exist?(journal_path))
      refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
    end
  end

  def test_archive_abandoned_shared_master_keeps_unpublished_sealed_head_after_tracking_commit
    with_shared_master_archive_fixture do |runner, workspace, slug, published|
      other = File.join(workspace, 'work', '2026-06-07-other', 'state.md')
      FileUtils.mkdir_p(File.dirname(other))
      File.write(other, "---\nlifecycle: active\n---\n\nUnpublished tracking checkpoint.\n")
      assert_git_success('git', '-C', workspace, 'add', other)
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'record unpublished tracking checkpoint')
      sealed = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip
      refute_equal(published, sealed)

      runner.archive(slug, as_is: true, abandoned: true)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(sealed, manifest.fetch('repositories').fetch(0).fetch('final_head_sha'))
      refute_equal(sealed, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip)
      assert_equal(published, git_capture_success('git', '-C', workspace, 'rev-parse', 'refs/remotes/origin/master').strip)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
    end
  end

  def test_archive_shared_master_retry_refuses_rewound_or_missing_local_and_origin_heads
    %i[local_rewound origin_rewound origin_missing].each do |change|
      with_shared_master_archive_fixture do |runner, workspace, slug, sealed|
        runner.define_singleton_method(:commit_tracking_transition!) do |*_args, **_options|
          raise DevSession::Error, 'pause before archive tracking commit'
        end
        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        assert_includes(error.message, 'pause before archive tracking commit')
        runner.singleton_class.remove_method(:commit_tracking_transition!)
        journal_path = runner.send(:lifecycle_journal_file, slug, 'archive')
        saved_journal = File.binread(journal_path)
        manifest_path = File.join(workspace, 'archive', slug, 'portal.yml')
        saved_manifest = File.binread(manifest_path)
        parent = git_capture_success('git', '-C', workspace, 'rev-parse', "#{sealed}^").strip
        common = change == :local_rewound ? File.join(workspace, '.git') : File.join(workspace, '.git', 'test-origin.git')
        if change == :origin_missing
          assert_git_success('git', "--git-dir=#{common}", 'update-ref', '-d', 'refs/heads/master')
        else
          assert_git_success('git', "--git-dir=#{common}", 'update-ref', 'refs/heads/master', parent)
        end

        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }

        assert_includes(error.message, 'no longer retains sealed HEAD') unless change == :origin_missing
        assert_equal(saved_journal, File.binread(journal_path))
        assert_equal(saved_manifest, File.binread(manifest_path))
        assert(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
      end
    end
  end

  def test_archive_cleanup_removes_nested_default_and_detached_checkouts_and_empty_containers
    with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
      group = File.dirname(feature)
      integration = File.join(group, 'integration-targets', 'sample')
      detached = File.join(group, 'review', 'sample')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', integration, 'master')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', detached, 'master')
      source = File.join(workspace, 'source-sample')
      File.write(File.join(source, 'later.txt'), 'later default commit')
      assert_git_success('git', '-C', source, 'add', 'later.txt')
      assert_git_success('git', '-C', source, 'commit', '-m', 'advance origin default')
      create_bare_repo(workspace, 'auxiliary')
      auxiliary = File.join(workspace, 'repos', 'auxiliary.git')
      assert_git_success('git', "--git-dir=#{auxiliary}", 'fetch', 'origin', '+refs/heads/master:refs/remotes/origin/master')
      assert_git_success('git', "--git-dir=#{auxiliary}", 'symbolic-ref', 'refs/remotes/origin/HEAD', 'refs/remotes/origin/master')
      assert_git_success('git', "--git-dir=#{auxiliary}", 'worktree', 'add', '--detach', File.join(group, 'other', 'auxiliary'), 'master')
      FileUtils.mkdir_p(File.join(group, 'empty', 'child'))
      unrelated = File.join(workspace, 'worktrees', slug + '-other', 'sample')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', unrelated, 'master')
      heads_before = git_capture_success('git', "--git-dir=#{common}", 'for-each-ref', '--format=%(refname):%(objectname)', 'refs/heads/')

      runner.archive(slug, as_is: true)

      refute(File.exist?(group))
      assert(File.directory?(unrelated))
      assert_equal(heads_before, git_capture_success('git', "--git-dir=#{common}", 'for-each-ref', '--format=%(refname):%(objectname)', 'refs/heads/'))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(['sample'], manifest.fetch('repositories').map { |repository| repository.fetch('name') })
      refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
    end
  end

  def test_archive_cleanup_refuses_unknown_foreign_locked_prunable_and_overlapping_state
    %i[file symlink unregistered foreign locked prunable overlap tracked_dirty untracked].each do |scenario|
      with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
        group = File.dirname(feature)
        case scenario
        when :file then File.write(File.join(group, 'unknown.txt'), 'keep')
        when :symlink then File.symlink(File.join(workspace, 'work'), File.join(group, 'shortcut'))
        when :unregistered then assert_git_success('git', 'init', File.join(group, 'unknown'))
        when :foreign
          foreign_root = File.join(workspace, 'foreign')
          FileUtils.mkdir_p(File.join(foreign_root, 'repos'))
          create_bare_repo(foreign_root, 'sample')
          assert_git_success('git', "--git-dir=#{foreign_root}/repos/sample.git", 'worktree', 'add', '--detach', File.join(group, 'foreign'), 'master')
        when :locked then assert_git_success('git', "--git-dir=#{common}", 'worktree', 'lock', feature)
        when :prunable then FileUtils.rm_rf(feature)
        when :overlap then assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', File.join(feature, 'nested'), 'master')
        when :tracked_dirty then File.write(File.join(feature, 'README.md'), 'changed')
        when :untracked then File.write(File.join(feature, 'untracked.txt'), 'keep')
        end

        modes = %i[tracked_dirty untracked].include?(scenario) ? [false, true] : [true]
        modes.each do |abandoned|
          assert_raises(DevSession::Error, scenario.to_s) { runner.archive(slug, as_is: true, abandoned:) }
        end

        assert(File.directory?(File.join(workspace, 'work', slug)), scenario)
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')), scenario)
        assert(File.directory?(feature), scenario) unless scenario == :prunable
      end
    end
  end

  def test_archive_cleanup_refuses_orphan_detached_commits_even_when_abandoned
    with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
      detached = File.join(File.dirname(feature), 'detached')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', detached, 'master')
      configure_git_identity(detached)
      File.write(File.join(detached, 'orphan.txt'), 'orphan')
      assert_git_success('git', '-C', detached, 'add', 'orphan.txt')
      assert_git_success('git', '-C', detached, 'commit', '-m', 'orphan')
      head = git_capture_success('git', '-C', detached, 'rev-parse', 'HEAD').strip
      refs = git_capture_success('git', "--git-dir=#{common}", 'for-each-ref', '--format=%(refname):%(objectname)')

      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, abandoned: true) }

      assert_includes(error.message, 'orphan detached')
      assert_includes(error.message, head)
      assert(File.directory?(detached))
      assert_equal(refs, git_capture_success('git', "--git-dir=#{common}", 'for-each-ref', '--format=%(refname):%(objectname)'))
    end
  end

  def test_archive_cleanup_keeps_registered_missing_checkout_merge_obligations
    with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
      configure_git_identity(feature)
      File.write(File.join(feature, 'feature.txt'), 'unmerged')
      assert_git_success('git', '-C', feature, 'add', 'feature.txt')
      assert_git_success('git', '-C', feature, 'commit', '-m', 'unmerged')
      head = git_capture_success('git', '-C', feature, 'rev-parse', 'HEAD').strip
      assert_git_success('git', "--git-dir=#{common}", 'push', 'origin', "refs/heads/#{slug}:refs/heads/#{slug}")
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'remove', feature)

      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      assert_includes(error.message, 'feature head is not merged')
      assert_git_success('git', "--git-dir=#{common}", 'push', 'origin', "refs/heads/#{slug}:refs/heads/master")

      runner.archive(slug, as_is: true)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(head, manifest.fetch('repositories').fetch(0).fetch('final_head_sha'))
    end
  end

  def test_archive_cleanup_additional_feature_requires_publication_but_abandoned_retains_it
    with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
      extra = File.join(File.dirname(feature), 'integration-targets', 'extra')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '-b', 'additional-feature', extra, 'master')
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      assert_includes(error.message, 'feature branch is not present on origin')

      runner.archive(slug, as_is: true, abandoned: true)

      assert_git_success('git', "--git-dir=#{common}", 'show-ref', '--verify', 'refs/heads/additional-feature')
      refute(File.exist?(extra))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(['sample'], manifest.fetch('repositories').map { |repository| repository.fetch('name') })
      assert(manifest.fetch('repositories').fetch(0).fetch('final_head_sha'))
    end
  end

  def test_archive_cleanup_prepared_intent_reserves_the_slug_and_retries_the_same_operation
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, feature|
      fail_publication = true
      runner.define_singleton_method(:write_lifecycle_journal) do |path, journal, **options|
        if fail_publication && options[:command] == 'archive' && options[:create]
          raise DevSession::Error, 'injected journal publication interruption'
        end
        super(path, journal, **options)
      end
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      cleanup = DevSession::ArchiveCleanup.new(runner, slug)
      intent = cleanup.load
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert(File.directory?(feature))
      assert_equal(false, intent.fetch('completed'))
      error = assert_raises(DevSession::Error) { runner.worktree_remove(slug, 'sample', as_is: true, force: false) }
      assert_includes(error.message, 'session archive is unfinished')
      fail_publication = false
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, operation_id: 'f' * 64) }
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, abandoned: true) }

      runner.archive(slug, as_is: true, operation_id: intent.fetch('operation_id'))

      observations = File.readlines(File.join(workspace, 'archive-observations.jsonl')).map { |line| JSON.parse(line) }
      reconstructed = observations.find do |observation|
        args = observation.fetch('args')
        !observation.fetch('journal_present') && args.include?('--expected-operation-id')
      end
      refute_nil(reconstructed, 'prepared retry must observe before republishing its journal')
      args = reconstructed.fetch('args')
      assert_equal(intent.fetch('operation_id'), args.fetch(args.index('--expected-operation-id') + 1))
      assert_equal(intent.fetch('mode'), args.fetch(args.index('--expected-archive-mode') + 1))
      refute(File.exist?(cleanup.path))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_archive_cleanup_reconciles_removal_before_progress_write_and_preserves_abandoned_heads
    with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
      head = git_capture_success('git', '-C', feature, 'rev-parse', 'HEAD').strip
      fail_after_removal = true
      runner.define_singleton_method(:archive_cleanup_remove!) do |repository, path|
        super(repository, path)
        if fail_after_removal
          fail_after_removal = false
          raise DevSession::Error, 'injected removal/progress interruption'
        end
      end
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, abandoned: true) }
      sidecar = DevSession::ArchiveCleanup.new(runner, slug).load
      assert_equal(false, sidecar.fetch('inventory').fetch('worktrees').fetch(0).fetch('removed'))
      refute(File.exist?(feature))
      assert_git_success('git', "--git-dir=#{common}", 'show-ref', '--verify', "refs/heads/#{slug}")

      runner.archive(slug, as_is: true, abandoned: true)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(head, manifest.fetch('repositories').fetch(0).fetch('final_head_sha'))
      refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
    end
  end

  def test_archive_cleanup_prepared_intent_refuses_changed_inventory_or_tracking
    %i[checkout tracking].each do |change|
      with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
        runner.define_singleton_method(:write_lifecycle_journal) do |path, journal, **options|
          raise DevSession::Error, 'pause before journal' if options[:command] == 'archive' && options[:create]
          super(path, journal, **options)
        end
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        if change == :checkout
          assert_git_success('git', "--git-dir=#{common}", 'worktree', 'remove', feature)
        else
          File.open(File.join(workspace, 'work', slug, 'plan.md'), 'a') { |file| file.write("\nChanged.\n") }
        end

        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }

        assert_includes(error.message, 'prepared archive cleanup')
        assert(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
        assert(File.directory?(File.join(workspace, 'work', slug)))
      end
    end
  end

  def test_archive_cleanup_reconciles_container_removal_before_progress_write
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, feature|
      container = File.join(File.dirname(feature), 'empty', 'child')
      FileUtils.mkdir_p(container)
      cleanup = DevSession::ArchiveCleanup.new(runner, slug)
      interrupt_progress = true
      cleanup.define_singleton_method(:write) do |sidecar, **options|
        if interrupt_progress && sidecar.fetch('inventory').fetch('containers').any? { |record| record.fetch('removed') }
          interrupt_progress = false
          raise DevSession::Error, 'container removal/progress interruption'
        end
        super(sidecar, **options)
      end
      with_archive_cleanup_instance(cleanup) do
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      end
      refute(File.exist?(container))
      recorded = cleanup.load.fetch('inventory').fetch('containers').find { |record| record.fetch('path') == 'empty/child' }
      assert_equal(false, recorded.fetch('removed'))

      runner.archive(slug, as_is: true)

      refute(File.exist?(File.dirname(feature)))
      refute(File.exist?(cleanup.path))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_archive_cleanup_refuses_a_removed_path_recreated_at_the_same_head
    with_archive_cleanup_fixture do |runner, _workspace, slug, common, feature|
      head = git_capture_success('git', '-C', feature, 'rev-parse', 'HEAD').strip
      cleanup = DevSession::ArchiveCleanup.new(runner, slug)
      interrupt_progress = true
      cleanup.define_singleton_method(:write) do |sidecar, **options|
        super(sidecar, **options)
        if interrupt_progress && sidecar.fetch('inventory').fetch('worktrees').any? { |record| record.fetch('removed') }
          interrupt_progress = false
          raise DevSession::Error, 'pause after durable removal progress'
        end
      end
      with_archive_cleanup_instance(cleanup) do
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      end
      assert_equal(true, cleanup.load.fetch('inventory').fetch('worktrees').fetch(0).fetch('removed'))
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', feature, slug)
      assert_equal(head, git_capture_success('git', '-C', feature, 'rev-parse', 'HEAD').strip)

      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }

      assert_includes(error.message, 'removed path reappeared')
      assert(File.directory?(feature))
      assert(File.exist?(cleanup.path))
    end
  end

  def test_archive_cleanup_refuses_replacement_or_added_files_after_sealing
    %i[admin directory file head].each do |scenario|
      with_archive_cleanup_fixture do |runner, workspace, slug, _common, feature|
        interrupted = true
        runner.define_singleton_method(:write_lifecycle_journal) do |path, journal, **options|
          super(path, journal, **options)
          if interrupted && options[:command] == 'archive' && options[:create]
            interrupted = false
            raise DevSession::Error, 'injected after seal'
          end
        end
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        case scenario
        when :admin
          admin = git_capture_success('git', '-C', feature, 'rev-parse', '--git-dir').strip
          saved = File.join(workspace, 'saved-admin')
          FileUtils.mv(admin, saved)
          FileUtils.cp_r(saved, admin)
        when :directory
          saved = File.join(workspace, 'saved-checkout')
          FileUtils.mv(feature, saved)
          FileUtils.cp_r(saved, feature)
        when :file
          File.write(File.join(File.dirname(feature), 'new.txt'), 'keep')
        when :head
          configure_git_identity(feature)
          File.write(File.join(feature, 'changed.txt'), 'later feature head')
          assert_git_success('git', '-C', feature, 'add', 'changed.txt')
          assert_git_success('git', '-C', feature, 'commit', '-m', 'change sealed feature head')
        end

        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }

        assert(File.directory?(feature))
        assert(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
      end
    end
  end

  def test_archive_cleanup_completed_receipt_recovers_without_repeating_the_tracking_commit
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, _feature|
      store = runner.send(:coordination_store)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      stat = File.stat(File.join(workspace, 'work', slug))
      owner_id, reservation_id = SecureRandom.uuid, SecureRandom.uuid
      store.transaction do |data|
        data['owners'][owner_id] = {'id' => owner_id,
          'identity' => WorkspaceCoordination.digest([workspace, 'fixture', slug, stat.dev, stat.ino,
            manifest.dig('codex', 'thread_id'), WorkspaceCoordination.creation_identity(manifest)]),
          'workspace' => workspace, 'workspaceName' => 'fixture', 'slug' => slug,
          'thread' => nil, 'socket' => '/fixture', 'portalUrl' => 'https://fixture.test', 'bootstrap' => true, 'fence' => nil}
        data['reservations'][reservation_id] = {'id' => reservation_id, 'ownerId' => owner_id,
          'machine' => 'fixture:machine', 'target' => 'machine', 'state' => 'deployed_review',
          'revision' => 1, 'sequence' => store.sequence(data), 'mode' => 'deploy_first',
          'approvalId' => nil, 'integrationObligations' => [], 'history' => [
            {'attempt' => SecureRandom.uuid, 'kind' => 'machine_activate', 'artifact' => 'sha256:' + 'a' * 64,
             'coverage' => [], 'coverageContracts' => [], 'observed' => {}, 'outcome' => 'applied'}]}
      end
      interrupt_completion = true
      runner.define_singleton_method(:archive_cleanup_check_completed!) do |*arguments, **options|
        if interrupt_completion && options[:recorded_retirement]
          interrupt_completion = false
          raise DevSession::Error, 'injected journal deletion/sidecar deletion interruption'
        end
        super(*arguments, **options)
      end
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      cleanup = DevSession::ArchiveCleanup.new(runner, slug)
      assert_equal(true, cleanup.load.fetch('completed'))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      # A predecessor marked completion but retained deployed reservations.
      store.transaction { |data| data['reservations'][reservation_id]['state'] = 'blocked_recovery' }
      failed_store = Object.new
      failed_store.define_singleton_method(:lifecycle_completed) { |*args, **options| raise WorkspaceCoordination::Error, 'injected completion replay failure' }
      runner.define_singleton_method(:coordination_store) { failed_store }
      assert_raises(WorkspaceCoordination::Error) { runner.archive(slug, as_is: true) }
      assert(File.exist?(cleanup.path), 'completion failure discarded recovery evidence')
      assert_equal 'blocked_recovery', store.read['reservations'][reservation_id]['state']
      runner.define_singleton_method(:coordination_store) { store }
      head = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, abandoned: true) }
      assert(File.exist?(cleanup.path))

      runner.archive(slug, as_is: true)

      assert_equal(head, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip)
      assert_equal 'released', store.read['reservations'][reservation_id]['state']
      refute(File.exist?(cleanup.path))
    end
  end

  def test_archive_cleanup_reproves_detached_retention_after_removal
    %i[advance delete rewind].each do |change|
      with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
        detached = File.join(File.dirname(feature), 'detached')
        assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', detached, 'master')
        configure_git_identity(detached)
        File.write(File.join(detached, 'retained.txt'), 'retained')
        assert_git_success('git', '-C', detached, 'add', 'retained.txt')
        assert_git_success('git', '-C', detached, 'commit', '-m', 'retained auxiliary')
        head = git_capture_success('git', '-C', detached, 'rev-parse', 'HEAD').strip
        assert_git_success('git', "--git-dir=#{common}", 'update-ref', 'refs/heads/retained-auxiliary', head)
        fail_after_removal = true
        runner.define_singleton_method(:archive_cleanup_remove!) do |repository, path|
          super(repository, path)
          if fail_after_removal && path == detached
            fail_after_removal = false
            raise DevSession::Error, 'interrupt after detached removal'
          end
        end
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, abandoned: true) }
        refute(File.exist?(detached))
        cleanup = DevSession::ArchiveCleanup.new(runner, slug)
        sealed = cleanup.load
        proof = sealed.fetch('inventory').fetch('proofs').find { |item| item.fetch('kind') == 'detached' }
        assert_equal('refs/heads/retained-auxiliary', proof.fetch('ref'))

        case change
        when :advance
          writer = File.join(workspace, 'retained-writer')
          assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', writer, 'retained-auxiliary')
          configure_git_identity(writer)
          File.write(File.join(writer, 'later.txt'), 'descendant retains sealed commit')
          assert_git_success('git', '-C', writer, 'add', 'later.txt')
          assert_git_success('git', '-C', writer, 'commit', '-m', 'advance retained ref')
          later = git_capture_success('git', '-C', writer, 'rev-parse', 'HEAD').strip
          assert_git_success('git', "--git-dir=#{common}", 'worktree', 'remove', writer)
          runner.archive(slug, as_is: true, abandoned: true)
          assert_equal(later, git_capture_success('git', "--git-dir=#{common}", 'rev-parse', 'refs/heads/retained-auxiliary').strip)
          assert(File.directory?(File.join(workspace, 'archive', slug)))
          refute(File.exist?(cleanup.path))
        when :delete, :rewind
          if change == :delete
            assert_git_success('git', "--git-dir=#{common}", 'update-ref', '-d', 'refs/heads/retained-auxiliary')
          else
            parent = git_capture_success('git', "--git-dir=#{common}", 'rev-parse', "#{head}^").strip
            assert_git_success('git', "--git-dir=#{common}", 'update-ref', 'refs/heads/retained-auxiliary', parent)
          end
          assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, abandoned: true) }

          assert(File.directory?(feature))
          assert(File.exist?(cleanup.path))
          refute(File.exist?(File.join(workspace, 'archive', slug)))
        end
      end
    end
  end

  def test_archive_cleanup_legacy_no_sidecar_journal_recovers_only_the_old_layout
    %i[plain nested].each do |layout|
      with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
        plan = runner.send(:prepare_cleanup, slug, force: false)
        heads = runner.send(:prove_registered_branches_merged!, slug, plan)
        runner.send(:prepare_archive_journal!, slug, 'complete', heads, plan)
        refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
        if layout == :nested
          assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', File.join(File.dirname(feature), 'review', 'sample'), 'master')
          assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
          assert(File.directory?(feature))
        else
          runner.archive(slug, as_is: true)
          assert(File.directory?(File.join(workspace, 'archive', slug)))
        end
      end
    end
  end

  def test_archive_cleanup_automatic_inventory_accepts_nested_auxiliary_and_preserves_the_rule
    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-automatic-cleanup'
      runner = automatic_fixture(workspace, slug)
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
      common = File.join(workspace, 'repos', 'sample.git')
      nested = File.join(workspace, 'worktrees', slug, 'review', 'sample')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', nested, 'master')

      snapshot = runner.send(:auto_archive_snapshot, slug)

      assert_equal('merged', snapshot.fetch('tier'))
      assert_equal([], snapshot.fetch('blockers'))
      assert(File.directory?(nested))
      refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
    end
  end

  def test_archive_cleanup_sidecar_rejects_duplicate_unknown_and_changed_inventory_fields
    with_archive_cleanup_fixture do |runner, _workspace, slug, _common, _feature|
      runner.define_singleton_method(:write_lifecycle_journal) do |path, journal, **options|
        raise DevSession::Error, 'pause before journal' if options[:command] == 'archive' && options[:create]
        super(path, journal, **options)
      end
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      cleanup = DevSession::ArchiveCleanup.new(runner, slug)
      original = File.binread(cleanup.path)
      duplicate = original.sub('"schema":1', '"schema":1,"schema":1')
      unknown = JSON.parse(original).merge('unknown' => true)
      changed = JSON.parse(original)
      changed.fetch('inventory').fetch('worktrees').fetch(0)['head'] = 'a' * 40
      escaped = JSON.parse(original)
      escaped.fetch('inventory').fetch('worktrees').fetch(0)['path'] = '../another-session'
      malformed_ref = JSON.parse(original)
      malformed_ref.fetch('inventory').fetch('proofs').fetch(0)['ref'] = 'refs/heads/bad name'
      malformed_row = JSON.parse(original)
      malformed_row.fetch('inventory')['proofs'] = [nil]
      bad_digest = JSON.parse(original).merge('inventory_sha256' => 'bad')
      oversized = ' ' * (DevSession::ArchiveCleanup::MAX_BYTES + 1)
      [duplicate, JSON.generate(unknown), JSON.generate(changed), JSON.generate(escaped),
       JSON.generate(malformed_ref), JSON.generate(malformed_row), JSON.generate(bad_digest), oversized].each do |encoded|
        File.binwrite(cleanup.path, encoded)
        assert_raises(DevSession::Error) { cleanup.load }
      end
      File.binwrite(cleanup.path, original)
      File.chmod(0o644, cleanup.path)
      assert_raises(DevSession::Error) { cleanup.load }
      File.chmod(0o600, cleanup.path)
      assert(cleanup.load)
    end
  end

  def test_explicit_archive_recovers_dirty_unpublished_checkout_and_detached_auxiliary
    with_archive_cleanup_fixture do |runner, workspace, slug, common, feature|
      File.write(File.join(feature, 'staged'), "staged content\n")
      assert_git_success('git', '-C', feature, 'add', 'staged')
      File.write(File.join(feature, 'staged'), "working content\n")
      File.binwrite(File.join(feature, 'untracked'), "\x00\xff".b)
      File.write(File.join(feature, '.gitignore'), "ignored\n")
      File.write(File.join(feature, 'ignored'), 'retained ignored content')
      auxiliary = File.join(workspace, 'worktrees', slug, 'nested', 'detached')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', auxiliary, 'master')
      File.write(File.join(auxiliary, 'local'), 'detached local content')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'lock', auxiliary)
      assert_git_success('git', "--git-dir=#{common}", 'config', 'remote.origin.url', '/unavailable-origin')
      runner.archive(slug, as_is: true, explicit: true)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal(3, manifest['schema'])
      assert_equal('archived', runner.send(:lifecycle_state, File.read(File.join(workspace, 'archive', slug, 'state.md'))))
      message = git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%B')
      assert_includes(message, 'explicitly archived session')
      refute_includes(message, 'feature heads are merged')
      recovery = runner.archive_cleanup_recovery(slug, manifest.fetch('archive_operation_id'))
      assert_equal(2, recovery.records.length)
      assert_git_success('git', "--git-dir=#{common}", 'gc', '--prune=now')
      runner.revive(slug, as_is: true)
      assert_equal("staged content\n", git_capture_success('git', '-C', feature, 'show', ':staged'))
      assert_equal("working content\n", File.binread(File.join(feature, 'staged')))
      assert_equal("\x00\xff".b, File.binread(File.join(feature, 'untracked')))
      assert_equal('retained ignored content', File.read(File.join(feature, 'ignored')))
      assert_equal('detached local content', File.read(File.join(auxiliary, 'local')))
      assert_equal(1, YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))['schema'])
    end
  end

  def test_explicit_archive_retry_preserves_saved_policy_after_forced_removal
    %w[active complete].each do |lifecycle|
      with_archive_cleanup_fixture(lifecycle:) do |runner, workspace, slug, _common, feature|
        File.write(File.join(feature, 'untracked'), 'recover me')
        runner.define_singleton_method(:commit_tracking_transition!) { |*, **| raise DevSession::Error, 'pause before commit' }
        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
        assert_equal('pause before commit', error.message)
        journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
        assert_equal('archived', journal.fetch('mode'))
        runner.singleton_class.remove_method(:commit_tracking_transition!)
        resumed_operation = nil
        runner.define_singleton_method(:finish_archive_journal!) do |current_slug, current, **options|
          resumed_operation = current.fetch('operation_id')
          super(current_slug, current, **options)
        end
        runner.archive(slug, as_is: true, explicit: true)
        assert_equal(journal.fetch('operation_id'), resumed_operation)
        refute(File.exist?(feature))
        assert(File.directory?(File.join(workspace, 'archive', slug)))
        manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
        assert_equal(journal.fetch('operation_id'), manifest.fetch('archive_operation_id'))
        runner.revive(slug, as_is: true)
        assert_equal('recover me', File.read(File.join(feature, 'untracked')))
      end
    end
  end

  def test_explicit_archive_revives_completed_and_abandoned_sources_with_local_changes
    %w[complete abandoned].each do |lifecycle|
      with_archive_cleanup_fixture(lifecycle:) do |runner, workspace, slug, common, feature|
        branch = git_capture_success('git', '-C', feature, 'symbolic-ref', 'HEAD').strip
        head = git_capture_success('git', '-C', feature, 'rev-parse', 'HEAD').strip
        File.write(File.join(feature, 'staged'), "staged content\n")
        assert_git_success('git', '-C', feature, 'add', 'staged')
        File.write(File.join(feature, 'staged'), "working content\n")
        File.binwrite(File.join(feature, 'untracked'), "\x00\xff".b)
        File.write(File.join(feature, '.gitignore'), "ignored\n")
        File.write(File.join(feature, 'ignored'), 'ignored content')
        runner.archive(slug, as_is: true, explicit: true)
        archive = File.join(workspace, 'archive', slug)
        assert_equal('archived', runner.send(:lifecycle_state, File.read(File.join(archive, 'state.md'))))
        assert_equal(head, git_capture_success('git', "--git-dir=#{common}", 'rev-parse', branch).strip)
        refute(File.exist?(feature))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
        refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
        runner.revive(slug, as_is: true)
        assert_equal('active', runner.send(:lifecycle_state, File.read(File.join(workspace, 'work', slug, 'state.md'))))
        assert_equal(branch, git_capture_success('git', '-C', feature, 'symbolic-ref', 'HEAD').strip)
        assert_equal(head, git_capture_success('git', '-C', feature, 'rev-parse', 'HEAD').strip)
        assert_equal("staged content\n", git_capture_success('git', '-C', feature, 'show', ':staged'))
        assert_equal("working content\n", File.binread(File.join(feature, 'staged')))
        assert_equal("\x00\xff".b, File.binread(File.join(feature, 'untracked')))
        assert_equal('ignored content', File.read(File.join(feature, 'ignored')))
      end
    end
  end

  def test_completed_restore_never_overwrites_later_edits_after_interruption
    [false, true].each do |edited|
      with_archive_cleanup_fixture do |runner, workspace, slug, _common, feature|
        File.write(File.join(feature, 'local'), 'saved content')
        runner.archive(slug, as_is: true, explicit: true)
        manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
        recovery = runner.archive_cleanup_recovery(slug, manifest.fetch('archive_operation_id'))
        record = recovery.records.first
        receipt = nil
        recovery.define_singleton_method(:write_json) do |path, contents|
          super(path, contents)
          if File.basename(path) == 'restored.json'
            receipt = path
            raise DevSession::Error, 'interrupted after completion receipt'
          end
        end
        assert_raises(DevSession::Error) { recovery.restore!(record) }
        recovery.singleton_class.remove_method(:write_json)
        intent = File.join(File.dirname(receipt), 'restoring.json')
        assert(File.file?(receipt))
        assert(File.file?(intent))
        if edited
          File.write(File.join(feature, 'local'), 'later operator edit')
          error = assert_raises(DevSession::Error) { recovery.restore!(record) }
          assert_includes(error.message, 'completed archive recovery checkout changed')
          assert_equal('later operator edit', File.read(File.join(feature, 'local')))
        else
          recovery.restore!(record)
          refute(File.exist?(intent))
          assert_equal('saved content', File.read(File.join(feature, 'local')))
        end
      end
    end
  end

  def test_archive_accepts_multiple_integration_targets_in_one_canonical_repository
    with_archive_cleanup_fixture do |runner, workspace, slug, common, _feature|
      release_target = File.join(workspace, 'worktrees', slug, 'release-target')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '-b', 'release', release_target, 'master')
      configure_git_identity(release_target)
      File.write(File.join(release_target, 'release-only'), 'release change')
      assert_git_success('git', '-C', release_target, 'add', 'release-only')
      assert_git_success('git', '-C', release_target, 'commit', '-m', 'release fixture')
      assert_git_success('git', '-C', release_target, 'push', 'origin', 'release')
      runner.worktree_add(slug, 'sample', as_is: true, name: 'release', branch: "#{slug}-release", base: 'release', fetch: false)
      path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(path))
      manifest.fetch('repositories').find { |entry| entry['name'] == 'release' }['default_branch'] = 'release'
      File.write(path, YAML.dump(manifest))
      commit_archive_fixture(workspace, slug, lifecycle: 'active')
      detached = File.join(workspace, 'worktrees', slug, 'nested', 'release-detached')
      assert_git_success('git', "--git-dir=#{common}", 'worktree', 'add', '--detach', detached, 'release')
      runner.archive(slug, as_is: true)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      assert_equal('complete', runner.send(:lifecycle_state, File.read(File.join(workspace, 'archive', slug, 'state.md'))))
    end
  end

  def test_git_symlink_placeholders_use_full_copy_without_changing_external_permissions
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, checkout|
      sentinel = File.join(workspace, 'outside-sentinel')
      File.write(sentinel, 'external contents')
      File.chmod(0o640, sentinel)
      placeholder = File.join(checkout, 'link-placeholder')
      File.symlink(sentinel, placeholder)
      assert_git_success('git', '-C', checkout, 'add', 'link-placeholder')
      assert_git_success('git', '-C', checkout, 'commit', '-m', 'tracked symlink fixture')
      File.unlink(placeholder)
      File.write(placeholder, sentinel)
      assert_git_success('git', '-C', checkout, 'config', 'core.symlinks', 'false')
      assert_empty(git_capture_success('git', '-C', checkout, 'status', '--porcelain').strip)
      before = git_capture_success('git', '-C', checkout, 'ls-files', '--stage')
      runner.archive(slug, as_is: true, explicit: true)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      recovery = runner.archive_cleanup_recovery(slug, manifest.fetch('archive_operation_id'))
      saved, = recovery.send(:read_record, recovery.records.first)
      assert_equal('files', saved.fetch('kind'))
      runner.revive(slug, as_is: true)
      refute(File.symlink?(placeholder))
      assert(File.file?(placeholder))
      assert_equal(sentinel, File.read(placeholder))
      assert_equal(before, git_capture_success('git', '-C', checkout, 'ls-files', '--stage'))
      assert_equal(0o640, File.stat(sentinel).mode & 0o777)
      assert_equal('external contents', File.read(sentinel))
    end
  end

  private

  def with_archive_cleanup_instance(cleanup)
    original = DevSession::ArchiveCleanup.method(:new)
    DevSession::ArchiveCleanup.define_singleton_method(:new) { |_runner, _slug| cleanup }
    yield
  ensure
    DevSession::ArchiveCleanup.define_singleton_method(:new, original)
  end

  def with_archive_cleanup_fixture(lifecycle: 'active')
    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-cleanup'
      runner = archive_runner_for(workspace)
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
      commit_archive_fixture(workspace, slug, lifecycle:)
      configure_workspace_origin(workspace)
      common = File.join(workspace, 'repos', 'sample.git')
      assert_git_success('git', "--git-dir=#{common}", 'config', 'user.name', 'Test User')
      assert_git_success('git', "--git-dir=#{common}", 'config', 'user.email', 'test@example.invalid')
      feature = File.join(workspace, 'worktrees', slug, 'sample')
      yield runner, workspace, slug, common, feature
    end
  end

  def with_shared_master_archive_fixture
    with_workspace do |workspace|
      slug = '2026-06-06-shared-master'
      runner = archive_runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_archive_fixture(workspace, slug, lifecycle: 'active')
      path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(path))
      manifest['repositories'] = [{ 'name' => 'workspace', 'project' => 'workspace', 'branch' => 'master', 'default_branch' => 'master' }]
      File.write(path, YAML.dump(manifest))
      assert_git_success('git', '-C', workspace, 'add', path)
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'register shared master obligation')
      configure_workspace_origin(workspace)
      sealed = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip
      yield runner, workspace, slug, sealed
    end
  end
end
