# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
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
      head = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, abandoned: true) }
      assert(File.exist?(cleanup.path))

      runner.archive(slug, as_is: true)

      assert_equal(head, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip)
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

  private

  def with_archive_cleanup_instance(cleanup)
    original = DevSession::ArchiveCleanup.method(:new)
    DevSession::ArchiveCleanup.define_singleton_method(:new) { |_runner, _slug| cleanup }
    yield
  ensure
    DevSession::ArchiveCleanup.define_singleton_method(:new, original)
  end

  def with_archive_cleanup_fixture
    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-cleanup'
      runner = runner_for(workspace)
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      common = File.join(workspace, 'repos', 'sample.git')
      feature = File.join(workspace, 'worktrees', slug, 'sample')
      yield runner, workspace, slug, common, feature
    end
  end
end
