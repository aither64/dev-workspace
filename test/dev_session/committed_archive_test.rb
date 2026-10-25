# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_committed_archive_discards_local_files_and_revives_saved_head
    with_archive_cleanup_fixture do |runner, workspace, slug, common, checkout|
      runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
      head = git_capture_success('git', '-C', checkout, 'rev-parse', 'HEAD').strip
      File.write(File.join(checkout, '.gitignore'), "target/\n")
      File.write(File.join(checkout, 'staged'), 'not committed')
      assert_git_success('git', '-C', checkout, 'add', 'staged')
      File.write(File.join(checkout, 'unstaged'), 'not committed either')
      FileUtils.mkdir_p(File.join(checkout, 'target'))
      File.write(File.join(checkout, 'target', 'cache'), 'rebuild this')
      tracking = File.join(workspace, 'work', slug)
      File.write(File.join(tracking, 'raw.log'), 'discard evidence')
      File.write(File.join(tracking, 'review.md'), 'retain explanation')
      runner.archive(slug, as_is: true, explicit: true)
      refute File.exist?(checkout)
      refute File.exist?(File.join(workspace, 'archive', slug, 'raw.log'))
      assert File.file?(File.join(workspace, 'archive', slug, 'review.md'))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      recovery = runner.archive_cleanup_recovery(slug, manifest.fetch('archive_operation_id'))
      record = recovery.records.first
      saved, destination = recovery.send(:read_record, record)
      assert_equal 'committed', saved.fetch('kind')
      refute File.exist?(File.join(destination, 'files'))
      refute File.exist?(File.join(destination, 'admin'))
      assert_equal head, git_capture_success('git', "--git-dir=#{common}", 'rev-parse', saved.fetch('head_ref')).strip
      runner.revive(slug, as_is: true)
      assert_equal head, git_capture_success('git', '-C', checkout, 'rev-parse', 'HEAD').strip
      assert_empty git_capture_success('git', '-C', checkout, 'status', '--porcelain', '--ignored=matching')
      %w[staged unstaged target].each { |name| refute File.exist?(File.join(checkout, name)) }
    end
  end

  def test_committed_tracking_maintenance_supersedes_only_synced_archive_digest
    [false, true].each do |synced|
      with_archive_cleanup_fixture do |runner, workspace, slug, _common, checkout|
        runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
        File.write(File.join(workspace, 'work', slug, 'raw.log'), 'legacy evidence')
        projection = WorkspaceTrackingSummaries.method(:prune)
        begin
          WorkspaceTrackingSummaries.define_singleton_method(:prune) { |*, **| [] }
          runner.archive(slug, as_is: true, explicit: true)
        ensure
          WorkspaceTrackingSummaries.define_singleton_method(:prune, projection)
        end
        runner.tracking_sync(slug, force: true) if synced
        assert_equal(synced ? 'synced' : 'pending', runner.tracking_sync_receipt(slug).fetch('state'))
        tracking = File.join(workspace, 'archive', slug)
        WorkspaceTrackingSummaries.prune(tracking, apply: true)
        assert_raises(DevSession::Error) { runner.send(:validate_archive_committed!, slug) }
        assert_git_success('git', '-C', workspace, 'add', '-A', File.join('archive', slug))
        assert_git_success('git', '-C', workspace, 'commit', '-m', 'compact archived tracking')
        if synced
          runner.revive(slug, as_is: true)
          assert File.directory?(checkout)
          refute File.exist?(File.join(workspace, 'work', slug, 'raw.log'))
        else
          error = assert_raises(DevSession::Error) { runner.send(:validate_archive_committed!, slug) }
          assert_includes error.message, 'locally archived tracking changed'
        end
      end
    end
  end

  def test_automatic_archive_projects_registered_raw_artifacts_after_eligibility
    with_workspace do |workspace|
      slug = '2026-06-06-summary-eligibility'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
      tracking = File.join(workspace, 'work', slug)
      File.write(File.join(tracking, 'raw.log'), 'disposable evidence')
      manifest_path = File.join(tracking, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['artifacts'] = [{'path' => 'raw.log', 'label' => 'verification'}]
      File.write(manifest_path, YAML.dump(manifest))
      assert_equal 'complete', automatic_scan(runner).fetch(0)['tier']
      age_automatic_session(runner, slug, WorkspaceAutoArchive::Policy::TIERS.fetch('complete') + 1)
      result = automatic_scan(runner).fetch(0)
      assert_equal 'archived', result['result'], result.inspect
      refute File.exist?(tracking)
      archive = File.join(workspace, 'archive', slug)
      refute File.exist?(File.join(archive, 'raw.log'))
      assert_empty YAML.safe_load(File.read(File.join(archive, 'portal.yml')))['artifacts']
    end
  end

  def test_summary_pruning_preserves_markdown_and_does_not_follow_links
    Dir.mktmpdir do |root|
      tracking = File.join(root, 'tracking')
      FileUtils.mkdir_p(File.join(tracking, 'logs'))
      File.write(File.join(root, 'outside'), 'keep outside')
      File.symlink(File.join(root, 'outside'), File.join(tracking, 'raw.link'))
      File.write(File.join(tracking, 'logs', 'review.md'), 'keep review')
      File.write(File.join(tracking, 'logs', 'raw.json'), '{}')
      File.write(File.join(tracking, 'portal.yml'), YAML.dump('repositories' => [], 'artifacts' => [{'path' => 'logs/review.md'}, {'path' => 'logs/raw.json'}]))
      before = Dir[File.join(tracking, '**', '*')]
      assert_equal 2, WorkspaceTrackingSummaries.prune(tracking).length
      assert_equal before, Dir[File.join(tracking, '**', '*')]
      WorkspaceTrackingSummaries.prune(tracking, apply: true)
      assert_equal 'keep outside', File.read(File.join(root, 'outside'))
      assert_equal [{'path' => 'logs/review.md'}], YAML.safe_load(File.read(File.join(tracking, 'portal.yml')))['artifacts']
      assert_empty WorkspaceTrackingSummaries.prune(tracking, apply: true)
    end
  end

  def test_completed_legacy_archive_compacts_before_discarding_file_payloads
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, checkout|
      enable_legacy_archive_capture(runner)
      File.write(File.join(checkout, 'untracked'), 'old local backup')
      runner.archive(slug, as_is: true, explicit: true)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      recovery = runner.archive_cleanup_recovery(slug, manifest.fetch('archive_operation_id'))
      record = recovery.records.first
      _saved, destination = recovery.send(:read_record, record)
      assert File.file?(File.join(destination, 'files', 'untracked'))
      2.times { recovery.compact! }
      assert_equal 'committed', recovery.send(:read_record, record).first.fetch('kind')
      refute File.exist?(File.join(destination, 'files'))
      runner.revive(slug, as_is: true)
      assert File.file?(File.join(checkout, 'README.md'))
      refute File.exist?(File.join(checkout, 'untracked'))
    end
  end

  def test_committed_revival_preserves_an_advanced_branch
    with_archive_cleanup_fixture do |runner, _workspace, slug, common, checkout|
      head = git_capture_success('git', '-C', checkout, 'rev-parse', 'HEAD').strip
      branch = git_capture_success('git', '-C', checkout, 'symbolic-ref', 'HEAD').strip
      runner.archive(slug, as_is: true, explicit: true)
      tree = git_capture_success('git', "--git-dir=#{common}", 'rev-parse', "#{head}^{tree}").strip
      advanced, error, status = Open3.capture3('git', "--git-dir=#{common}", 'commit-tree', tree, '-p', head, stdin_data: "advanced branch\n")
      assert status.success?, error
      assert_git_success('git', "--git-dir=#{common}", 'update-ref', branch, advanced.strip)
      runner.revive(slug, as_is: true)
      assert_equal head, git_capture_success('git', '-C', checkout, 'rev-parse', 'HEAD').strip
      assert_equal advanced.strip, git_capture_success('git', "--git-dir=#{common}", 'rev-parse', branch).strip
      _out, _error, status = Open3.capture3('git', '-C', checkout, 'symbolic-ref', '--quiet', 'HEAD')
      refute status.success?
    end
  end

end
