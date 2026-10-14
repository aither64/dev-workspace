# frozen_string_literal: true
require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def sensitive_fixture(workspace, slug)
    assert_git_success('git', 'init', '-b', 'master', workspace)
    configure_git_identity(workspace)
    File.write(File.join(workspace, '.gitignore'), "worktrees/\n.xdg-state/\n.archive-authority/\n.archive-codex/\narchive-observer.rb\n")
    assert_git_success('git', '-C', workspace, 'add', '.gitignore')
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'workspace fixture')
    runner = archive_runner_for(workspace)
    runner.send(:select_sensitivity!, slug, requested: true)
    runner.ensure_tracking_files(slug)
    runner.send(:ensure_portal_manifest, slug, tracking_only: true)
    runner
  end

  def test_sensitive_tracking_is_local_and_exclusions_preserve_other_rules
    with_workspace do |workspace|
      runner = sensitive_fixture(workspace, 'sensitive')
      exclude = File.join(workspace, '.git', 'info', 'exclude')
      File.open(exclude, 'a') { |f| f.puts('/unrelated-local/') }
      runner.send(:select_sensitivity!, 'second', requested: true)
      assert_includes(File.read(exclude), '/unrelated-local/')
      assert_equal(1, File.read(exclude).lines.count { |line| line == "/work/sensitive/\n" })
      # Journal privacy must not depend on a workspace-wide worktrees ignore.
      ignore = File.join(workspace, '.gitignore')
      File.write(ignore, File.read(ignore).sub("worktrees/\n", ''))
      assert_git_success('git', '-C', workspace, 'add', '.gitignore')
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'exercise local exclusions')
      File.write(runner.send(:creation_journal_file, 'sensitive'), 'private fixture')
      assert_git_success('git', '-C', workspace, 'add', '-A')
      assert_empty(git_capture_success('git', '-C', workspace, 'diff', '--cached', '--name-only'))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work/sensitive/portal.yml')))
      assert_equal(true, manifest['sensitive'])
      assert_equal(0o600, File.stat(runner.send(:sensitivity_file, 'sensitive')).mode & 0o777)
      assert(runner_for(workspace).send(:sensitive_session?, 'sensitive'))
    end
  end

  def test_sensitive_archive_retries_and_revives_without_git_commits
    with_workspace do |workspace|
      slug = 'sensitive'
      runner = sensitive_fixture(workspace, slug)
      before = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD')
      File.write(File.join(workspace, 'sentinel'), 'staged')
      assert_git_success('git', '-C', workspace, 'add', 'sentinel')
      # Simulate interruption after the local transition has completed.
      original = runner.method(:commit_tracking_transition!)
      interrupted = false
      runner.define_singleton_method(:commit_tracking_transition!) do |*a, **kw|
        original.call(*a, **kw)
        unless interrupted
          interrupted = true
          raise DevSession::Error, 'interrupted after local transition'
        end
      end
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
      runner.singleton_class.remove_method(:commit_tracking_transition!)
      runner.archive(slug, as_is: true, explicit: true)
      assert_equal(before, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD'))
      assert(runner.send(:sensitivity_record, slug)['archived'])
      runner.define_singleton_method(:start) { |*, **| nil }
      runner.revive(slug, as_is: true)
      assert_equal(true, YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))['sensitive'])
      assert_equal(before, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD'))
      assert_equal("sentinel\n", git_capture_success('git', '-C', workspace, 'diff', '--cached', '--name-only'))
    end
  end

  def test_sensitive_selection_refuses_existing_tracking_and_overridden_ignore
    with_workspace do |workspace|
      runner = sensitive_fixture(workspace, 'first')
      runner.ensure_tracking_files('ordinary')
      assert_raises(DevSession::Error) { runner.send(:select_sensitivity!, 'ordinary', requested: true) }
      File.open(File.join(workspace, '.gitignore'), 'a') { |f| f.puts('!/work/blocked/') }
      assert_raises(DevSession::Error) { runner.send(:select_sensitivity!, 'blocked', requested: true) }
      refute(File.exist?(File.join(workspace, 'work/blocked/plan.md')))
    end
  end

  def test_sensitive_selection_refuses_specific_journal_exceptions
    with_workspace do |workspace|
      runner = sensitive_fixture(workspace, 'seed')
      ignore = File.join(workspace, '.gitignore')
      File.write(ignore, File.read(ignore).sub("worktrees/\n", ''))
      %w[creation fork start archive removal revive archive-cleanup].each do |kind|
        slug = "blocked-#{kind}"
        File.open(ignore, 'a') { |f| f.puts("!/worktrees/.locks/#{slug}.#{kind}.json") }
        error = assert_raises(DevSession::Error) { runner.send(:select_sensitivity!, slug, requested: true) }
        assert_includes error.message, "#{slug}.#{kind}.json"
        refute File.exist?(File.join(workspace, 'work', slug, 'plan.md'))
      end
      slug = 'blocked-temporary'
      File.open(ignore, 'a') { |f| f.puts("!/worktrees/.locks/#{slug}.fork.json.*.tmp") }
      assert_raises(DevSession::Error) { runner.send(:select_sensitivity!, slug, requested: true) }
    end
  end

  def test_sensitive_archive_tombstone_survives_missing_archive_files
    with_workspace do |workspace|
      runner = sensitive_fixture(workspace, 'tombstone')
      runner.archive('tombstone', as_is: true, explicit: true)
      FileUtils.rm_rf(File.join(workspace, 'archive', 'tombstone'))
      assert_raises(DevSession::Error) { runner.ensure_tracking_files('tombstone') }
      assert_includes(runner.send(:known_slugs), 'tombstone')
    end
  end

  def test_sensitive_choice_survives_creation_interruption_and_seals_journal
    with_workspace do |workspace|
      runner = sensitive_fixture(workspace, 'seed')
      # The durable selection precedes all tracking writes, so an interruption
      # at that boundary can resume without a new flag or changed privacy.
      runner.send(:select_sensitivity!, 'new-session', requested: true)
      fresh = runner_for(workspace)
      fresh.send(:select_sensitivity!, 'new-session')
      assert_includes(fresh.send(:known_slugs), 'new-session')
      goal = File.join(workspace, '.goal')
      File.write(goal, 'private request')
      journal = fresh.send(:prepare_creation_journal, 'new-session', goal,
        exclusive: true, run_codex: true, model: nil, effort: nil)
      assert_equal true, journal['sensitive']
      again = fresh.send(:prepare_creation_journal, 'new-session', goal,
        exclusive: true, run_codex: true, model: nil, effort: nil)
      assert_equal journal, again
      journal['sensitive'] = false
      File.write(fresh.send(:creation_journal_file, 'new-session'), JSON.generate(journal))
      assert_raises(DevSession::Error) do
        fresh.send(:prepare_creation_journal, 'new-session', goal,
          exclusive: true, run_codex: true, model: nil, effort: nil)
      end
    end
  end

  def test_sensitive_delete_retains_private_recovery_without_committing
    with_workspace do |workspace|
      runner = sensitive_fixture(workspace, 'discard')
      before = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD')
      runner.delete('discard', as_is: true, force: false)
      refute File.exist?(File.join(workspace, 'work', 'discard'))
      assert runner.send(:sensitive_session?, 'discard')
      assert_equal before, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD')
    end
  end

  def test_sensitive_records_cannot_be_forced_into_index_by_lifecycle
    with_workspace do |workspace|
      slug = 'sensitive'
      runner = sensitive_fixture(workspace, slug)
      assert_git_success('git', '-C', workspace, 'add', '-f', "work/#{slug}/plan.md")
      assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
      assert_equal("work/#{slug}/plan.md\n", git_capture_success('git', '-C', workspace, 'diff', '--cached', '--name-only'))
    end
  end
end
