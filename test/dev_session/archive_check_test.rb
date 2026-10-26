# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_fresh_archive_reader_loads_live_authority_without_rewriting_it
    with_workspace do |workspace|
      slug = '2026-06-06-live-archive-reader'
      authority_dir = File.join(workspace, '.authority')
      tmux = ManagedTmux.new(slug, workspace:, socket_path: '/run/test.sock', id: '$11')
      writer = archive_runner_for(workspace, tmux:, authority_dir:)
      writer.ensure_tracking_files(slug)
      commit_archive_fixture(workspace, slug, lifecycle: 'active')
      writer.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      path = File.join(authority_dir, "#{slug}.json")
      before = File.binread(path)
      reader = archive_runner_for(workspace, tmux:, authority_dir:)
      result = reader.archive_check(slug, as_is: true)
      assert(result.fetch('ready'), result.fetch('blockers').inspect)
      assert_equal(before, File.binread(path))
      refute(tmux.killed)

      legacy = JSON.parse(before)
      legacy.delete('tmux_identity')
      File.write(path, JSON.generate(legacy) + "\n")
      before = File.binread(path)
      reader = archive_runner_for(workspace, tmux:, authority_dir:)
      result = reader.archive_check(slug, as_is: true)
      refute(result.fetch('ready'))
      assert(result.fetch('blockers').any? { |item| item['message'].include?('lacks a tmux creation identity') })
      assert_equal(before, File.binread(path))
      assert_equal('a' * 64, tmux.session(slug).identity_token)
      refute(tmux.killed)
    end
  end

  def test_archive_reader_does_not_initialize_a_missing_authority_directory
    with_workspace do |workspace|
      slug = '2026-06-06-no-authority-reader'
      runner = archive_runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_archive_fixture(workspace, slug, lifecycle: 'active')
      directory = File.join(workspace, '.missing-authority')
      reader = archive_runner_for(workspace, authority_dir: directory)
      reader.archive_check(slug, as_is: true)
      refute(File.exist?(directory))
    end
  end

  def test_archive_reader_accepts_owned_readonly_removal_parents_without_changing_them
    with_archive_cleanup_fixture do |runner, workspace, slug, _common, _checkout|
      group = File.join(workspace, 'worktrees', slug)
      nested = File.join(group, 'nested')
      FileUtils.mkdir_p(File.join(nested, 'empty'))
      runner.define_singleton_method(:quiesce_explicit_archive!) { |_| flunk('preflight stopped agents') }
      [group, nested].each do |path|
        File.chmod(0o555, path)
        result = runner.archive_check(slug, as_is: true)
        assert(result.fetch('ready'), result.fetch('blockers').inspect)
        assert_equal(0o555, File.stat(path).mode & 0o777)
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
        File.chmod(0o755, path)
      end
    ensure
      [group, nested].compact.each { |path| File.chmod(0o755, path) if File.directory?(path) }
    end
  end

  def test_readonly_checkout_cache_is_repairable_without_creating_retention_refs
    with_archive_cleanup_fixture do |runner, _workspace, slug, common, checkout|
      cache = File.join(checkout, '.gems', 'bin')
      FileUtils.mkdir_p(cache)
      File.write(File.join(cache, 'overcommit'), 'generated')
      File.chmod(0o555, cache)
      before = git_capture_success('git', "--git-dir=#{common}", 'for-each-ref')
      result = runner.archive_check(slug, as_is: true)
      assert(result.fetch('ready'), result.fetch('blockers').inspect)
      assert_equal(0o555, File.stat(cache).mode & 0o777)
      assert_equal(before, git_capture_success('git', "--git-dir=#{common}", 'for-each-ref'))
      File.chmod(0o755, cache)
      assert(runner.archive_check(slug, as_is: true).fetch('ready'))
    ensure
      File.chmod(0o755, cache) if cache && File.directory?(cache)
    end
  end

  def test_collects_loose_files_without_quiescing_or_publishing_archive_evidence
    with_workspace do |workspace|
      slug = '2026-06-06-check'
      runner = automatic_fixture(workspace, slug)
      directory = File.join(workspace, 'worktrees', slug, '.hook-gems', 'bin')
      FileUtils.mkdir_p(directory)
      %w[overcommit rubocop].each { |name| File.write(File.join(directory, name), 'generated') }
      File.chmod(0o555, directory)
      runner.define_singleton_method(:quiesce_explicit_archive!) { |_| flunk('preflight stopped agents') }
      refs = git_capture_success('git', '-C', workspace, 'show-ref')
      result = runner.archive_check(slug, as_is: true)
      refute(result.fetch('ready'))
      assert_equal(2, result.fetch('blockerCount'))
      assert_equal(%w[overcommit rubocop], result.fetch('blockers').map { |item| File.basename(item.fetch('path')) })
      assert(result.fetch('blockers').all? { |item| item.fetch('code') == 'unmanaged_entry' })
      assert_equal(refs, git_capture_success('git', '-C', workspace, 'show-ref'))
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
      assert_includes(error.message, 'archive readiness checks failed')
      refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    ensure
      File.chmod(0o755, directory) if directory && File.directory?(directory)
    end
  end

  def test_ready_check_does_not_pin_refs_or_modify_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-ready-check'
      runner = automatic_fixture(workspace, slug)
      tracking = Dir[File.join(workspace, 'work', slug, '*')].to_h { |path| [path, File.binread(path)] }
      refs = git_capture_success('git', '-C', workspace, 'show-ref')
      result = runner.archive_check(slug, as_is: true)
      assert(result.fetch('ready'), result.fetch('blockers').inspect)
      assert_equal(6, result.fetch('passedChecks'))
      assert_equal(0, result.fetch('blockerCount'))
      assert_equal(refs, git_capture_success('git', '-C', workspace, 'show-ref'))
      assert_equal(tracking, tracking.keys.to_h { |path| [path, File.binread(path)] })
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end
end
