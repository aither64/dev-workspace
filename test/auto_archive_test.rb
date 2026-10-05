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
    { 'identity' => 'thread-1', 'root_thread_id' => 'root-1', 'fingerprint_version' => 1,
      'dimensions' => { 'activity' => fingerprint }, 'unknown_dimensions' => [],
      'activity_known' => true, 'idle' => blockers.empty?, 'tier' => tier,
      'diagnostics' => blockers.map { |message| WorkspaceAutoArchive::Policy.diagnostic('queued_input', 'busy', message) } }
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

  def test_submission_unknown_loses_continuity_across_restart_with_identical_tokens
    first = observe
    unknown = snapshot.merge('activity_known' => false, 'idle' => false, 'dimensions' => {},
      'diagnostics' => [WorkspaceAutoArchive::Policy.diagnostic('submission_unverified', 'activity_unknown', 'Submissions cannot be proved resolved.')])
    failed = observe(first, unknown, policy, NOW + 86_400)
    assert(failed['continuity_lost'])
    assert_nil(failed['idle_since'])
    refute(failed['eligible'])
    restarted = JSON.parse(JSON.generate(failed))
    recovered = observe(restarted, snapshot, policy, NOW + 172_800)
    assert_equal((NOW + 172_800).iso8601, recovered['idle_since'])
    refute(recovered['eligible'])
  end

  def test_proof_errors_preserve_known_grace_and_unknown_dimension_never_proves_eligibility
    first = observe
    proof = snapshot.merge('diagnostics' => [WorkspaceAutoArchive::Policy.diagnostic('merge_unverified', 'merge_proof', 'Merge proof is unavailable.')])
    blocked = observe(first, proof, policy, NOW + 86_400)
    assert_equal(first['idle_since'], blocked['idle_since'])
    refute(blocked['eligible'])
    changed_text = proof.merge('diagnostics' => [WorkspaceAutoArchive::Policy.diagnostic('merge_unverified', 'merge_proof', 'Network lookup failed again.')])
    again = observe(blocked, changed_text, policy, NOW + 172_800)
    assert_equal(first['idle_since'], again['idle_since'])
    unknown = snapshot.merge('dimensions' => {}, 'unknown_dimensions' => ['activity'])
    refute(observe(again, unknown, policy, NOW + 172_800)['eligible'])
    assert(observe(again, snapshot, policy, NOW + 172_800)['eligible'])
  end

  def test_busy_to_idle_and_dirty_to_clean_restart_grace
    first = observe({}, snapshot.merge('idle' => false,
      'dimensions' => { 'activity' => 'queue-id', 'worktrees' => 'dirty' },
      'diagnostics' => [WorkspaceAutoArchive::Policy.diagnostic('queued_input', 'busy', 'Queued input.')]))
    clean = snapshot.merge('dimensions' => { 'activity' => 'unchanged', 'worktrees' => 'clean' })
    recovered = observe(first, clean, policy, NOW + 172_800)
    assert_equal((NOW + 172_800).iso8601, recovered['idle_since'])
    refute(recovered['eligible'])
  end

  def test_legacy_root_hold_converts_only_on_positive_same_root_and_starts_one_baseline
    old = { 'identity' => 'root-1', 'hold' => true, 'idle_since' => (NOW - 172_800).iso8601, 'fingerprint' => 'old' }
    converted = observe(old, snapshot)
    assert(converted['hold'])
    assert_equal(NOW.iso8601, converted['idle_since'])
    assert_equal(1, converted['fingerprint_version'])
    again = observe(converted, snapshot, policy, NOW + 100)
    assert_equal(converted['idle_since'], again['idle_since'])
    refute(observe(old, snapshot.merge('root_thread_id' => 'replacement'))['hold'])
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
