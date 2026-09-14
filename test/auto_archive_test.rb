# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require_relative '../libexec/workspace-auto-archive'

class AutoArchiveTest < Minitest::Test
  NOW = Time.utc(2026, 9, 14, 12)

  def policy(epoch = NOW)
    { 'enabled' => true, 'epoch' => epoch.iso8601 }
  end

  def snapshot(tier = 'complete', fingerprint = 'unchanged', blockers = [])
    { 'identity' => 'thread-1', 'fingerprint' => fingerprint, 'tier' => tier, 'blockers' => blockers }
  end

  def observe(previous = {}, value = snapshot, config = policy, now = NOW)
    WorkspaceAutoArchive::Policy.observe({ 'hold' => false }.merge(previous), value, config, now)
  end

  def test_all_tiers_require_the_exact_full_period
    { 'complete' => 86_400, 'merged' => 604_800, 'empty' => 1_209_600 }.each do |tier, seconds|
      first = observe({}, snapshot(tier))
      refute(first['eligible'])
      refute(observe(first, snapshot(tier), policy, NOW + seconds - 1)['eligible'])
      assert(observe(first, snapshot(tier), policy, NOW + seconds)['eligible'], tier)
    end
  end

  def test_changed_content_and_conversation_restart_the_period
    first = observe
    changed = observe(first, snapshot('complete', 'changed'), policy, NOW + 86_400)
    refute(changed['eligible'])
    assert_equal((NOW + 86_400).iso8601, changed['idle_since'])
    assert(observe(changed, snapshot('complete', 'changed'), policy, NOW + 172_800)['eligible'])
  end

  def test_unchanged_polling_and_restart_preserve_the_period
    first = JSON.parse(JSON.generate(observe))
    second = observe(first, snapshot, policy, NOW + 120)
    assert_equal(first['idle_since'], second['idle_since'])
    assert(observe(second, snapshot, policy, NOW + 86_400)['eligible'])
  end

  def test_holds_disable_and_blockers_prevent_archival
    old = observe
    refute(observe(old.merge('hold' => true), snapshot, policy, NOW + 172_800)['eligible'])
    refute(observe(old, snapshot, policy.merge('enabled' => false), NOW + 172_800)['eligible'])
    refute(observe(old, snapshot('complete', 'unchanged', ['Queued message.']), policy, NOW + 172_800)['eligible'])
    refute(observe(old, snapshot(nil), policy, NOW + 1_209_600)['eligible'])
  end

  def test_release_revival_and_reenable_start_fresh_periods
    old = observe
    reset = observe(old.merge('reset_at' => NOW.iso8601), snapshot, policy, NOW + 172_800)
    refute(reset['eligible'])
    assert_nil(reset['reset_at'])
    refute(observe(old, snapshot, policy(NOW + 60), NOW + 172_800)['eligible'])
    refute(observe(old, snapshot.merge('identity' => 'new-thread'), policy, NOW + 172_800)['eligible'])
  end

  def test_clock_regression_does_not_expire_a_period
    first = observe
    backwards = observe(first, snapshot, policy, NOW - 100)
    refute(backwards['eligible'])
    assert_equal((NOW - 100).iso8601, backwards['idle_since'])
  end

  def test_stores_are_bound_to_the_workspace_and_survive_restarts
    Dir.mktmpdir do |directory|
      workspace = File.join(directory, 'workspace')
      other = File.join(directory, 'other')
      FileUtils.mkdir_p([workspace, other])
      store = WorkspaceAutoArchive::Store.new(state_root: directory, workspace:)
      refute(store.policy['enabled'])
      refute(File.exist?(store.root), 'reading absent state must not create it')
      store.lock do
        store.write('policy', policy)
        store.write('session-example', { 'slug' => 'example', 'hold' => true })
      end
      again = WorkspaceAutoArchive::Store.new(state_root: directory, workspace:)
      assert(again.policy['enabled'])
      assert(again.session('example')['hold'])
      assert_equal(0o600, File.stat(File.join(store.root, 'policy.json')).mode & 0o777)
      refute(WorkspaceAutoArchive::Store.new(state_root: directory, workspace: other).policy['enabled'])
      data = JSON.parse(File.read(File.join(store.root, 'policy.json')))
      data['workspace'] = other
      File.write(File.join(store.root, 'policy.json'), JSON.generate(data))
      assert_raises(WorkspaceAutoArchive::Error) { again.policy }
    end
  end

  def test_corrupt_and_future_schemas_fail_closed
    Dir.mktmpdir do |workspace|
      store = WorkspaceAutoArchive::Store.new(state_root: workspace, workspace:)
      store.write('policy', policy)
      path = File.join(store.root, 'policy.json')
      File.write(path, '{invalid')
      assert_raises(WorkspaceAutoArchive::Error) { store.policy }
      File.write(path, JSON.generate('schema' => 2, 'workspace' => workspace))
      assert_raises(WorkspaceAutoArchive::Error) { store.policy }
    end
  end
end
