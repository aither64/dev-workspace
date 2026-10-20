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

  def test_held_repository_claim_preserves_collectible_and_interrupted_payloads
    person, snapshot = prepare('claimed-collection')
    cancel(person, submit(person, snapshot))
    claim = @engine.acquire_claim(person, request('repositories' => ['code'], 'machines' => [], 'label' => 'ordinary Git mutation'))
    assert_empty collect(apply: true)['removed']
    assert File.directory?(source(snapshot))
    @engine.change_claim(person, claim['id'], 'release')
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
    claim = @engine.acquire_claim(person, request('repositories' => ['code'], 'machines' => [], 'label' => 'inspect interrupted collection'))
    assert_empty collect(apply: true)['removed']
    refute_empty WorkspaceCoordination::Retention.new(@engine.store).read['pending']
    @engine.change_claim(person, claim['id'], 'release')
    collect(apply: true)
    assert_empty WorkspaceCoordination::Retention.new(@engine.store).read['pending']
    refute File.exist?(source(snapshot))
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

  def integration_scratch(person, phase: 'complete')
    id = SecureRandom.uuid
    path = File.join(@engine.store.root, 'integrations', id, "code-#{@base[0, 12]}-#{SecureRandom.hex(6)}")
    FileUtils.mkdir_p(File.dirname(path))
    git('-C', @bare, 'worktree', 'add', '--detach', path, @base)
    run = {'schema' => 1, 'id' => id, 'ownerId' => person['id'], 'identity' => person['identity'],
      'phase' => phase, 'request' => {'repositories' => [{'repository' => 'code'}]}}
    save_integration(run)
    [run, path]
  end

  def save_integration(run)
    WorkspaceCoordination.atomic(File.join(@engine.store.root, 'integrations', "#{run['id']}.json"), run)
  end

  def worker_root(id, kind, package = @package)
    @engine.retain_root("#{id}-#{Digest::SHA256.hexdigest(File.realpath(package))[0, 12]}", kind, package)
    File.join(@engine.store.root, 'roots', "#{id}-#{Digest::SHA256.hexdigest(File.realpath(package))[0, 12]}-#{kind}")
  end

  def profile_generations(current: 5)
    File.unlink(@profile)
    1.upto(5) do |number|
      package = number == 5 ? @package : File.join(@tmp, "old-package-#{number}")
      FileUtils.mkdir_p(package)
      File.symlink(package, "#{@profile}-#{number}-link")
    end
    File.symlink(File.basename("#{@profile}-#{current}-link"), @profile)
    capture = @engine.method(:capture)
    @engine.define_singleton_method(:capture) do |*args, **options|
      if args.first == 'nix-env'
        raise 'unexpected profile mutation' unless args[1..3] == ['--profile', @profile, '--delete-generations']
        File.unlink("#{@profile}-#{args.fetch(4)}-link")
        ''
      else
        capture.call(*args, **options)
      end
    end
  end

  def test_completed_integration_scratch_is_collected_without_losing_journal_or_refs
    run, path = integration_scratch(owner('scratch'))
    journal = File.join(@engine.store.root, 'integrations', "#{run['id']}.json")
    before = File.binread(journal)
    refs = git('-C', @bare, 'show-ref')
    assert_equal [path], collect['actions'].map { |item| item['path'] }
    assert_equal before, File.binread(journal)
    assert_equal 1, collect(apply: true)['removed'].length
    refute File.exist?(path)
    assert_equal run, WorkspaceCoordination.private_read(journal)
    assert_equal refs, git('-C', @bare, 'show-ref')
    refute_includes git('-C', @bare, 'worktree', 'list', '--porcelain'), "worktree #{path}\n"
    assert_empty collect(apply: true)['removed']
  end

  def test_unfinished_or_locked_integration_preserves_its_scratch
    run, path = integration_scratch(owner('unfinished-scratch'), phase: 'prepare')
    assert_empty collect(apply: true)['removed']
    run['phase'] = 'complete'
    save_integration(run)
    @engine.store.lock("integration-#{run['id']}") do
      result = Thread.new { collect(apply: true) }.value
      assert result['retry']
      assert_empty result['removed']
    end
    assert File.directory?(path)
    collect(apply: true)
    refute File.exist?(path)
  end

  def test_edited_held_unknown_and_foreign_integration_data_survive
    person = owner('scratch-holds')
    _run, edited = integration_scratch(person)
    File.write(File.join(edited, 'local-data'), 'operator work')
    _run, held = integration_scratch(person)
    File.write(File.join(held, '.keep'), 'keep')
    run, foreign = integration_scratch(person)
    git('-C', @bare, 'worktree', 'remove', foreign)
    other = File.join(@tmp, 'other.git')
    git('clone', '--bare', @remote, other)
    git('-C', other, 'worktree', 'add', '--detach', foreign, @base)
    unknown = File.join(File.dirname(foreign), 'operator-data')
    FileUtils.mkdir_p(unknown)
    assert_empty collect(apply: true)['removed']
    [edited, held, foreign, unknown].each { |path| assert File.directory?(path) }
  end

  def test_process_and_live_artifact_references_keep_completed_scratch
    person, snapshot = prepare('scratch-reference')
    _run, path = integration_scratch(person)
    hook = @engine.save_artifact('kind' => 'merge_hooks', 'repository' => 'code',
      'featureHead' => snapshot.dig('operation', 'featureHead'), 'environment' => {'BUNDLE_PATH' => path})
    snapshot = @engine.prepare_snapshot(person, 'merge', snapshot['operation'].merge('hookArtifact' => hook['id']), evidence(person))
    job = submit(person, snapshot)
    collect(apply: true)
    assert File.directory?(path)
    cancel(person, job)
    pid = Process.spawn(RbConfig.ruby, '-e', 'sleep 60', chdir: path)
    begin
      collect(apply: true)
      assert File.directory?(path)
    ensure
      Process.kill('TERM', pid)
      Process.wait(pid)
    end
    collect(apply: true)
    refute File.exist?(path)
  end

  def test_integration_removal_recovers_after_git_has_removed_the_directory
    run, path = integration_scratch(owner('scratch-interruption'))
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
    refute File.exist?(path)
    assert @engine.store.integration_journals.first['payloadGcPending'].key?(File.basename(path))
    assert_equal 1, collect(apply: true)['removed'].length
    assert_equal run, @engine.store.integration_journals.first
  end

  def interrupt_scratch_removal(path, remove_git: false)
    capture = @engine.method(:capture)
    @engine.define_singleton_method(:capture) do |*args, **options|
      if args.include?('remove')
        File.unlink(File.join(path, 'base'))
        File.unlink(File.join(path, '.git')) if remove_git
        raise WorkspaceCoordination::Error, 'fixture partial deletion'
      end
      capture.call(*args, **options)
    end
    assert_raises(WorkspaceCoordination::Error) { collect(apply: true) }
  ensure
    @engine.define_singleton_method(:capture, capture)
  end

  def test_integration_removal_recovers_from_partial_deletion_with_or_without_git_file
    [false, true].each do |remove_git|
      run, path = integration_scratch(owner("partial-scratch-#{remove_git}"))
      interrupt_scratch_removal(path, remove_git: remove_git)
      assert File.directory?(path)
      assert_equal 1, collect(apply: true)['removed'].length
      refute File.exist?(path)
      assert_equal run, @engine.store.integration_journals.find { |item| item['id'] == run['id'] }
    end
  end

  def test_pending_integration_removal_preserves_changed_registration_and_explicit_holds
    person = owner('changed-pending-scratch')
    run, path = integration_scratch(person)
    interrupt_scratch_removal(path)
    File.write(File.join(path, '.keep'), 'hold pending removal')
    assert_empty collect(apply: true)['removed']
    File.unlink(File.join(path, '.keep'))
    git('-C', path, 'checkout', '-b', 'reused-scratch')
    result = collect(apply: true)
    assert_empty result['removed']
    assert result['preserved'].any? { |item| item['reason'] == 'interrupted integration checkout registration changed' }
    assert File.directory?(path)
    assert @engine.store.integration_journals.first['payloadGcPending'].key?(File.basename(path))
    git('-C', path, 'checkout', '--detach', @base)
    feature(person)
    git('-C', path, 'checkout', '--detach', person['slug'])
    assert_empty collect(apply: true)['removed']
    assert_equal run['id'], @engine.store.integration_journals.first['id']
  end

  def test_pending_integration_removal_does_not_delete_replacement_data_without_registration
    _run, path = integration_scratch(owner('replaced-pending-scratch'))
    interrupt_scratch_removal(path)
    git('-C', @bare, 'worktree', 'remove', '--force', path)
    FileUtils.mkdir_p(path)
    File.write(File.join(path, 'new-data'), 'replacement operator data')
    result = collect(apply: true)
    assert_empty result['removed']
    assert result['preserved'].any? { |item| item['reason'] == 'interrupted integration checkout registration is missing' }
    assert_equal 'replacement operator data', File.read(File.join(path, 'new-data'))
  end

  def test_pending_integration_removal_preserves_native_git_worktree_lock
    _run, path = integration_scratch(owner('locked-pending-scratch'))
    File.write(File.join(@bare, 'info', 'exclude'), "retained-cache\n")
    File.write(File.join(path, 'retained-cache'), 'retained contents')
    interrupt_scratch_removal(path, remove_git: true)
    git('-C', @bare, 'worktree', 'lock', '--reason', 'operator inspection', path)
    result = collect(apply: true)
    assert_empty result['removed']
    assert result['preserved'].any? { |item| item['reason'] == 'retained worktree is locked' }
    assert File.directory?(path)
    assert_equal 'retained contents', File.read(File.join(path, 'retained-cache'))
    git('-C', @bare, 'worktree', 'unlock', path)
    assert_equal 1, collect(apply: true)['removed'].length
    refute File.exist?(path)
  end

  def test_pending_integration_removal_rechecks_native_lock_before_directory_deletion
    _run, path = integration_scratch(owner('late-locked-pending-scratch'))
    File.write(File.join(@bare, 'info', 'exclude'), "retained-cache\n")
    File.write(File.join(path, 'retained-cache'), 'late-held contents')
    interrupt_scratch_removal(path, remove_git: true)
    capture = @engine.method(:capture)
    common = @bare
    lists = 0
    @engine.define_singleton_method(:capture) do |*args, **options|
      if args.include?('list') && (lists += 1) == 2
        capture.call('git', '--git-dir', common, 'worktree', 'lock', path)
      end
      capture.call(*args, **options)
    end
    error = assert_raises(WorkspaceCoordination::Error) { collect(apply: true) }
    assert_equal 'retained worktree is locked', error.message
    assert File.directory?(path)
    assert_equal 'late-held contents', File.read(File.join(path, 'retained-cache'))
  ensure
    @engine.define_singleton_method(:capture, capture) if capture
  end

  def test_stopped_notification_roots_are_released_even_with_pending_delivery
    person, snapshot = prepare('notification-roots')
    submit(person, snapshot)
    @engine.claim
    root = worker_root(person['id'], 'notification')
    assert @engine.store.read['outbox'].values.any? { |event| event['state'] != 'delivered' }
    collect(apply: true)
    refute File.symlink?(root)
    assert File.directory?(source(snapshot))
    assert_equal root, worker_root(person['id'], 'notification')
  end

  def test_notification_lock_and_active_service_protect_worker_roots
    person = owner('notification-lock')
    root = worker_root(person['id'], 'notification')
    @engine.store.lock("notifications-#{person['id']}") do
      result = Thread.new { collect(apply: true) }.value
      assert_empty result['removed']
    end
    assert File.symlink?(root)
    collector = WorkspaceCoordination::Collector.new(@engine)
    collector.define_singleton_method(:service_running?) { |_unit| true }
    assert_empty collector.call(apply: true)['removed']
    collect(apply: true)
    refute File.symlink?(root)
  end

  def test_background_worker_roots_follow_journal_completion_and_known_targets
    run, _path = integration_scratch(owner('integration-root'), phase: 'prepare')
    root = worker_root(run['id'], 'integration')
    collect(apply: true)
    assert File.symlink?(root)
    run['phase'] = 'complete'
    save_integration(run)
    collect(apply: true)
    refute File.symlink?(root)
    wrong = File.join(@engine.store.root, 'roots', "#{run['ownerId']}-#{'0' * 12}-notification")
    File.symlink(@package, wrong)
    assert_empty collect(apply: true)['removed']
    assert File.symlink?(wrong)
  end

  def test_profile_cleanup_keeps_current_predecessor_and_separate_codex_roots
    profile_generations
    codex = File.join(@state, 'codex', 'generations', '1')
    FileUtils.mkdir_p(File.dirname(codex))
    File.symlink(@package, codex)
    before = File.readlink(@profile)
    assert_equal [1, 2, 3], collect['actions'].map { |action| action['generation'] }
    assert File.symlink?("#{@profile}-1-link")
    assert_equal 3, collect(apply: true)['removed'].length
    assert_equal before, File.readlink(@profile)
    assert File.symlink?("#{@profile}-4-link")
    assert File.symlink?("#{@profile}-5-link")
    assert File.symlink?(codex)
    assert_empty collect(apply: true)['removed']
  end

  def test_profile_cleanup_preserves_newer_generations_after_a_legacy_rollback
    profile_generations(current: 3)
    assert_equal [1], collect(apply: true)['removed'].map { |action| action['generation'] }
    2.upto(5) { |number| assert File.symlink?("#{@profile}-#{number}-link") }
    assert_equal File.basename("#{@profile}-3-link"), File.readlink(@profile)
  end

  def test_profile_cleanup_retains_packages_referenced_by_live_preparations
    person, snapshot = prepare('profile-reference')
    old = File.join(@tmp, 'old-package-1')
    hook = @engine.save_artifact('kind' => 'merge_hooks', 'repository' => 'code',
      'featureHead' => snapshot.dig('operation', 'featureHead'), 'environment' => {'DEPENDENCY' => old})
    @engine.prepare_snapshot(person, 'merge', snapshot['operation'].merge('hookArtifact' => hook['id']), evidence(person))
    profile_generations
    removed = collect(apply: true)['removed'].select { |action| action['kind'] == 'profile_generation' }
    assert_equal [2, 3], removed.map { |action| action['generation'] }
    assert File.symlink?("#{@profile}-1-link")
  end

  def test_profile_selection_change_stops_further_generation_removal
    profile_generations
    capture = @engine.method(:capture)
    @engine.define_singleton_method(:capture) do |*args, **options|
      result = capture.call(*args, **options)
      if args.first == 'nix-env' && args.last == '1'
        File.unlink(@profile)
        File.symlink(File.basename("#{@profile}-2-link"), @profile)
      end
      result
    end
    error = assert_raises(WorkspaceCoordination::Error) { collect(apply: true) }
    assert_match(/profile changed/, error.message)
    refute File.symlink?("#{@profile}-1-link")
    assert File.symlink?("#{@profile}-2-link")
    assert_equal File.basename("#{@profile}-2-link"), File.readlink(@profile)
  end

  def test_running_preparation_defers_all_collection
    _run, path = integration_scratch(owner('preparation-guard'))
    collector = WorkspaceCoordination::Collector.new(@engine)
    collector.define_singleton_method(:preparation_processes) { [12345] }
    result = collector.call(apply: true)
    assert result['retry']
    assert_empty result['removed']
    assert File.directory?(path)
    assert_equal 12345, result['preserved'].first['pid']
  end

end
