# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require_relative '../libexec/workspace-coordination-execution'
require_relative 'support/coordination_fixture'

class CoordinationGcTest < Minitest::Test
  include CoordinationFixture

  def prepare(slug)
    person = owner(slug)
    feature(person)
    [person, @engine.prepare_merge(person, 'code', evidence(person))]
  end

  def submit(person, snapshot)
    @engine.mutate(person, 'submit', request('kind' => 'merge', 'dependsOn' => [],
      'approvalId' => approval(person, snapshot), 'reservationId' => nil, 'snapshot' => snapshot))
  end

  def cancel(person, job)
    payload = request('expectedRevision' => job['revision'], 'reason' => 'completed fixture')
    @engine.mutate(person, 'cancel', payload, job_id: job['id'])
    payload
  end

  def collect(apply: false)
    WorkspaceCoordination::Collector.new(@engine).call(apply: apply)
  end

  def source(snapshot)
    File.join(@engine.store.root, 'sources', snapshot.dig('operation', 'featureHead'), 'checkout')
  end

  def test_queued_work_and_valid_preparations_survive_without_extending_lease
    person, snapshot = prepare('live')
    index = File.join(@engine.store.root, 'retention.json')
    before = File.binread(index)
    assert_empty collect['actions']
    assert_equal before, File.binread(index)
    submit(person, snapshot)
    with_time(Time.now + 9 * 86_400) { assert_empty collect(apply: true)['removed'] }
    assert File.directory?(source(snapshot))
  end

  def test_finished_work_is_collected_immediately_with_metadata_and_receipt_replay
    person, snapshot = prepare('finished')
    job = submit(person, snapshot)
    payload = cancel(person, job)
    result = collect(apply: true)
    assert_equal 2, result['removed'].count { |item| item['kind'] == 'worktree' }
    refute File.exist?(source(snapshot))
    refute File.exist?(snapshot.dig('operation', 'targetCheckout'))
    saved = File.join(@engine.store.root, 'sources', snapshot['id'].delete_prefix('sha256:'), 'snapshot.json')
    assert File.file?(saved)
    assert_equal 'canceled', @engine.mutate(person, 'cancel', payload, job_id: job['id'])['state']
    assert_empty collect(apply: true)['removed']
  end

  def test_shared_source_survives_a_second_unsubmitted_preparation
    person, first = prepare('shared')
    job = submit(person, first)
    second = @engine.prepare_merge(person, 'code', evidence(person))
    cancel(person, job)
    collect(apply: true)
    assert File.directory?(source(first))
    assert File.directory?(second.dig('operation', 'targetCheckout'))
    refute File.exist?(first.dig('operation', 'targetCheckout'))
  end

  def test_submitted_work_can_refresh_and_execute_after_its_preparation_lease
    person, snapshot = prepare('delayed-worker')
    job = submit(person, snapshot)
    with_time(Time.now + 8 * 86_400) do
      @engine.mutate(person, 'refresh', request('expectedRevision' => job['revision'],
        'snapshot' => snapshot, 'approvalId' => approval(person, snapshot)), job_id: job['id'])
      claimed = @engine.claim.first
      saved_owner = @engine.store.read['owners'][claimed['ownerId']]
      value = {'schema' => 1, 'job' => claimed, 'owner' => saved_owner, 'package' => @package,
        'profile' => @profile, 'stateRoot' => @state, 'entry' => @entry,
        'workspace' => @workspace, 'catalogDigest' => @engine.catalog.digest}
      value['digest'] = WorkspaceCoordination.digest(value)
      path = @engine.attempt_path(claimed['attempt'], 'request.json')
      WorkspaceCoordination.atomic(path, value)
      WorkspaceCoordination::Worker.new(path, fixtures: true).run
      @engine.reconcile_attempts
      assert_equal 'succeeded', @engine.store.read['jobs'][job['id']]['state']
    end
  end

  def test_unused_preparation_expires_and_new_submission_requires_preparation
    person, snapshot = prepare('expired')
    with_time(Time.now + 8 * 86_400) do
      error = assert_raises(WorkspaceCoordination::Error) { approval(person, snapshot) }
      assert_match(/prepare this operation again/, error.message)
      assert_equal 2, collect(apply: true)['removed'].length
    end
    renewed = @engine.prepare_merge(person, 'code', evidence(person))
    assert File.directory?(source(renewed))
    assert_equal 'waiting_approval', submit(person, renewed)['state']
  end

  def test_legacy_unused_preparation_gets_adoption_grace_without_a_schema_change
    _person, snapshot = prepare('legacy')
    File.unlink(File.join(@engine.store.root, 'retention.json'))
    before = File.binread(File.join(@engine.store.root, 'ledger.json'))
    with_time(Time.now + 20 * 86_400) do
      assert_empty collect(apply: true)['removed']
      with_time(Time.now + 8 * 86_400) { assert_equal 2, collect(apply: true)['removed'].length }
    end
    assert_equal before, File.binread(File.join(@engine.store.root, 'ledger.json'))
    assert File.file?(File.join(@engine.store.root, 'sources', snapshot['id'].delete_prefix('sha256:'), 'snapshot.json'))
  end

  def test_batch_submission_checks_expiry_but_replays_committed_receipts
    person, snapshot = prepare('batch')
    payload = request('jobs' => [{'key' => 'merge', 'kind' => 'merge', 'dependsOn' => [],
      'approvalId' => approval(person, snapshot), 'reservationId' => nil, 'snapshot' => snapshot}])
    with_time(Time.now + 8 * 86_400) do
      error = assert_raises(WorkspaceCoordination::Error) { @engine.submit_batch(person, payload) }
      assert_match(/prepare this operation again/, error.message)
    end
    result = @engine.submit_batch(person, payload)
    cancel(person, result['jobs'].first)
    collect(apply: true)
    assert_equal result, @engine.submit_batch(person, payload)
  end

  def test_hold_and_local_changes_preserve_worktree_and_its_dependency_root
    person, snapshot = prepare('held')
    hook = @engine.save_artifact('kind' => 'merge_hooks', 'repository' => 'code',
      'featureHead' => snapshot.dig('operation', 'featureHead'), 'environment' => {'BUNDLE_PATH' => source(snapshot)})
    @engine.retain_root(hook['id'], 'dependency', @package)
    operation = snapshot['operation'].merge('hookArtifact' => hook['id'])
    snapshot = @engine.prepare_snapshot(person, 'merge', operation, evidence(person))
    job = submit(person, snapshot)
    cancel(person, job)
    File.write(File.join(source(snapshot), '.keep'), 'debugging')
    collect(apply: true)
    assert File.directory?(source(snapshot))
    assert File.symlink?(File.join(@engine.store.root, 'roots', "#{hook['id'].delete_prefix('sha256:')}-dependency"))
  end

  def test_unknown_root_and_source_layout_are_never_deleted
    FileUtils.mkdir_p(@engine.store.root, mode: 0o700)
    FileUtils.mkdir_p(File.join(@engine.store.root, 'sources', 'operator-data', 'checkout'))
    FileUtils.mkdir_p(File.join(@engine.store.root, 'roots'))
    root = File.join(@engine.store.root, 'roots', 'unknown-dependency')
    File.symlink(@package, root)
    assert_empty collect(apply: true)['removed']
    assert File.symlink?(root)
    assert File.directory?(File.join(@engine.store.root, 'sources', 'operator-data', 'checkout'))
  end

  def test_pending_integration_journal_keeps_a_finished_jobs_payloads
    person, snapshot = prepare('integration')
    job = submit(person, snapshot)
    cancel(person, job)
    directory = File.join(@engine.store.root, 'integrations')
    FileUtils.mkdir_p(directory)
    id = SecureRandom.uuid
    path = File.join(directory, "#{id}.json")
    WorkspaceCoordination.atomic(path, {'schema' => 1, 'id' => id, 'phase' => 'deploy', 'operation' => snapshot})
    assert_empty collect(apply: true)['removed']
    WorkspaceCoordination.atomic(path, {'schema' => 1, 'id' => id, 'phase' => 'complete', 'operation' => snapshot})
    assert_equal 2, collect(apply: true)['removed'].length
  end

  def test_worker_lock_keeps_a_terminal_jobs_payloads_until_exit
    person, snapshot = prepare('worker')
    job = submit(person, snapshot)
    cancel(person, job)
    attempt = SecureRandom.uuid
    @engine.store.transaction { |data| data['jobs'][job['id']]['attempt'] = attempt }
    directory = File.join(@engine.store.root, 'attempts', attempt)
    FileUtils.mkdir_p(directory)
    File.open(File.join(directory, 'worker.lock'), 'w') do |file|
      file.flock(File::LOCK_EX)
      assert_empty collect(apply: true)['removed']
    end
    assert_equal 2, collect(apply: true)['removed'].length
  end

  def test_integration_preparation_excludes_collection_and_retains_hook_dependencies
    person, snapshot = prepare('integration-preparation')
    cancel(person, submit(person, snapshot))
    id = SecureRandom.uuid
    journal = File.join(@engine.store.root, 'integrations', "#{id}.json")
    WorkspaceCoordination.atomic(journal, {'schema' => 1, 'id' => id, 'phase' => 'prepare',
      'head' => snapshot.dig('operation', 'featureHead')})
    hook = @engine.save_artifact('kind' => 'merge_hooks', 'repository' => 'code',
      'featureHead' => snapshot.dig('operation', 'featureHead'), 'environment' => {})
    @engine.retain_root(hook['id'], 'hook-0', @package)
    root = File.join(@engine.store.root, 'roots', "#{hook['id'].delete_prefix('sha256:')}-hook-0")
    # Hold the same lock as Integration#resume while it initializes dependencies.
    @engine.store.lock("integration-#{id}") do
      collector = Thread.new { collect(apply: true) }
      result = collector.value
      assert result['retry']
      assert_empty result['removed']
      assert_equal 'integration driver is running', result['preserved'].first['reason']
    end
    collect(apply: true)
    assert File.directory?(source(snapshot))
    assert File.symlink?(root)
    WorkspaceCoordination.atomic(journal, {'schema' => 1, 'id' => id, 'phase' => 'complete'})
    collect(apply: true)
    refute File.exist?(source(snapshot))
    refute File.symlink?(root)
  end

  def test_interruption_after_git_removal_resumes_without_losing_history
    person, snapshot = prepare('interrupted')
    cancel(person, submit(person, snapshot))
    capture = @engine.method(:capture)
    interrupted = false
    @engine.define_singleton_method(:capture) do |*args, **options|
      result = capture.call(*args, **options)
      if !interrupted && args.include?('remove')
        interrupted = true
        raise WorkspaceCoordination::Error, 'fixture interruption after deletion'
      end
      result
    end
    assert_raises(WorkspaceCoordination::Error) { collect(apply: true) }
    result = collect(apply: true)
    refute result['retry']
    assert_empty WorkspaceCoordination::Retention.new(@engine.store).read['pending']
    refute File.exist?(source(snapshot))
    assert_equal 'canceled', @engine.store.read['jobs'].values.first['state']
  end
  def test_live_reservation_retains_finished_payload_until_release
    person, snapshot = prepare('reservation')
    job = submit(person, snapshot)
    cancel(person, job)
    id = SecureRandom.uuid
    @engine.store.transaction do |data|
      data['reservations'][id] = {'id' => id, 'ownerId' => person['id'], 'machine' => 'fixture:machine',
        'target' => 'fixture', 'state' => 'deployed_review', 'revision' => 1, 'sequence' => 500,
        'mode' => 'deploy_first', 'approvalId' => SecureRandom.uuid, 'integrationObligations' => [],
        'history' => [{'attempt' => SecureRandom.uuid, 'kind' => 'merge', 'artifact' => snapshot['id'],
          'coverage' => [], 'coverageContracts' => [], 'observed' => nil, 'outcome' => 'applied'}]}
    end
    assert_empty collect(apply: true)['removed']
    @engine.store.transaction { |data| data['reservations'][id]['state'] = 'released' }
    assert_equal 2, collect(apply: true)['removed'].length
  end

  def test_process_cwd_preserves_a_finished_source
    person, snapshot = prepare('process-cwd')
    cancel(person, submit(person, snapshot))
    pid = Process.spawn(RbConfig.ruby, '-e', 'sleep 60', chdir: source(snapshot))
    begin
      collect(apply: true)
      assert File.directory?(source(snapshot))
    ensure
      Process.kill('TERM', pid)
      Process.wait(pid)
    end
    collect(apply: true)
    refute File.exist?(source(snapshot))
  end

  def test_collected_attempt_root_is_released_after_a_terminal_worker_exits
    person, snapshot = prepare('attempt-root')
    job = submit(person, snapshot)
    cancel(person, job)
    attempt = SecureRandom.uuid
    @engine.store.transaction { |data| data['jobs'][job['id']]['attempt'] = attempt }
    @engine.retain_root(attempt, 'worker', @package)
    collect(apply: true)
    refute File.symlink?(File.join(@engine.store.root, 'roots', "#{attempt}-worker"))
  end

  def test_background_journal_retains_live_payloads_and_allows_collection_after_completion
    person, snapshot = prepare('background-retention')
    cancel(person, submit(person, snapshot))
    id = SecureRandom.uuid
    path = File.join(@engine.store.root, 'integrations', "#{id}.json")
    value = {'schema' => 2, 'id' => id, 'ownerId' => person['id'], 'identity' => person['identity'],
      'phase' => 'deploy', 'steps' => {}, 'jobs' => {}, 'operation' => snapshot,
      'continuation' => {'state' => 'waiting', 'generation' => 0, 'observedRevision' => nil}}
    WorkspaceCoordination.atomic(path, value)
    assert_empty collect(apply: true)['removed']
    assert File.directory?(source(snapshot))
    value['phase'] = 'complete'
    value['continuation']['state'] = 'complete'
    WorkspaceCoordination.atomic(path, value)
    assert_equal 2, collect(apply: true)['removed'].length
    refute File.exist?(source(snapshot))
  end

end
