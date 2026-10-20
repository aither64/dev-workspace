# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require 'stringio'
require_relative '../libexec/workspace-coordination-execution'
require_relative 'support/coordination_fixture'
load File.expand_path('../libexec/workspace-host', __dir__)

class CoordinationClaimsTest < Minitest::Test
  include CoordinationFixture

  def setup
    super
    File.write(File.join(@package, 'share/dev-workspace/claim-contract.json'), JSON.generate(WorkspaceCoordination::CLAIM_CONTRACT))
  end

  def acquire(person, **fields)
    @engine.acquire_claim(person, request('repositories' => ['code'], 'machines' => [], 'label' => 'fixture ownership').merge(fields.transform_keys(&:to_s)))
  end

  def machines
    adapter = File.join(@tmp, 'adapter')
    File.write(adapter, "#!/bin/sh\nexit 0\n")
    File.chmod(0o700, adapter)
    @catalog['adapters']['fixture'] = {'protocol' => 1, 'command' => adapter}
    %w[first second].each do |name|
      @catalog['machines'][name] = {'id' => "fixture:#{name}", 'aliases' => ["alias-#{name}"],
        'adapter' => 'fixture', 'configurationRepository' => 'code', 'selector' => name}
    end
    install_catalog
    @engine = engine
  end

  def test_acquire_never_checks_out_executes_or_creates_payloads
    person = owner('ownership')
    before = git('-C', @bare, 'show-ref')
    item = acquire(person)
    assert_equal 'held', item['state']
    assert_equal before, git('-C', @bare, 'show-ref')
    assert_equal [], @engine.status(person['slug'])['jobs']
    assert_equal [item], @engine.status(person['slug'])['claims']
    %w[sources commands attempts].each { |name| refute File.exist?(File.join(@engine.store.root, name)) }
    assert_raises(WorkspaceCoordination::Error) { acquire(person, argv: ['true']) }
  end

  def test_fifo_grants_and_durable_notification_follow_explicit_release
    first, second, third = owner('first'), owner('second'), owner('third')
    held, waiting, later = acquire(first), acquire(second), acquire(third)
    assert_equal %w[held waiting waiting], [held, waiting, later].map { |item| item['state'] }
    @engine.change_claim(first, held['id'], 'release')
    @engine.tick_claims(third)
    assert_equal 'waiting', @engine.store.claims['claims'][later['id']]['state']
    @engine.tick_claims(second)
    assert_equal 'held', @engine.store.claims['claims'][waiting['id']]['state']
    events = @engine.store.read['outbox'].values
    assert_equal 1, events.length
    assert_equal second['id'], events.first['ownerId']
    assert_includes events.first['text'], waiting['id']
    @engine.tick_claims(second)
    assert_equal 1, @engine.store.read['outbox'].length
    @engine.change_claim(second, waiting['id'], 'release')
    # An undelivered or stale notice never restores released ownership.
    assert_equal 'released', @engine.store.claims['claims'][waiting['id']]['state']
    @engine.tick_claims(third)
    assert_equal 'held', @engine.store.claims['claims'][later['id']]['state']
  end

  def test_machine_holder_can_use_a_disjoint_repository_while_combined_request_waits
    machines
    holder, waiter = owner('machine-holder'), owner('combined-waiter')
    machine = acquire(holder, repositories: [], machines: ['first'])
    combined = acquire(waiter, machines: ['first'])
    assert_equal 'waiting', combined['state']
    repository = acquire(holder)
    assert_equal 'held', repository['state']
    @engine.change_claim(holder, repository['id'], 'release')
    @engine.change_claim(holder, machine['id'], 'release')
    @engine.tick_claims(waiter)
    assert_equal 'held', @engine.store.claims['claims'][combined['id']]['state']
  end

  def test_atomic_resources_and_disjoint_claims_with_machine_aliases
    machines
    first, second = owner('first'), owner('second')
    held = acquire(first, repositories: [], machines: ['alias-first'])
    waiting = acquire(second, machines: ['first'])
    assert_equal 'waiting', waiting['state']
    assert_equal ['machine:fixture:first'], @engine.store.held_claim_resources
    disjoint = acquire(first, repositories: [], machines: ['second'])
    assert_equal 'held', disjoint['state']
    assert_equal 'waiting', acquire(second, repositories: [], machines: ['alias-first'])['state']
    @engine.change_claim(first, held['id'], 'release')
    @engine.tick_claims(second)
    assert_equal 'held', @engine.store.claims['claims'][waiting['id']]['state']
  end

  def test_retry_release_and_cancel_are_owner_scoped_and_idempotent
    first, second = owner('first'), owner('second')
    held, waiting = acquire(first), acquire(second)
    assert_equal held, @engine.acquire_claim(first, held['request'])
    assert_raises(WorkspaceCoordination::Error) { @engine.acquire_claim(first, held['request'].merge('label' => 'changed')) }
    assert_raises(WorkspaceCoordination::Error) { @engine.acquire_claim(second, held['request']) }
    assert_raises(WorkspaceCoordination::Error) { @engine.change_claim(second, held['id'], 'release') }
    assert_raises(WorkspaceCoordination::Error) { @engine.change_claim(first, held['id'], 'cancel') }
    assert_raises(WorkspaceCoordination::Error) { @engine.change_claim(second, waiting['id'], 'release') }
    canceled = @engine.change_claim(second, waiting['id'], 'cancel')
    assert_equal canceled, @engine.change_claim(second, waiting['id'], 'cancel')
    released = @engine.change_claim(first, held['id'], 'release')
    assert_equal released, @engine.change_claim(first, held['id'], 'release')
    assert_equal released, @engine.acquire_claim(first, held['request'])
    @engine.tick_claims(second)
    assert_empty @engine.store.held_claim_resources
  end

  def test_restart_and_idle_do_not_expire_ownership
    first, second = owner('first'), owner('second')
    held = acquire(first)
    waiting = acquire(second)
    with_time(Time.now + 30 * 86_400) do
      @engine = engine
      @engine.tick_claims(second)
      assert_equal 'held', @engine.store.claims['claims'][held['id']]['state']
      assert_equal 'waiting', @engine.store.claims['claims'][waiting['id']]['state']
    end
    assert_raises(WorkspaceCoordination::Error) { @engine.change_claim(first.merge('id' => SecureRandom.uuid), held['id'], 'release') }
  end

  def test_canonical_repository_aliases_and_common_directory_exclude_each_other
    first, second = owner('first'), owner('second')
    @catalog['repositories']['alias'] = @catalog['repositories']['code']
    install_catalog
    File.symlink(@bare, File.join(@workspace, 'repos/alias.git'))
    path = File.join(@workspace, 'work', second['slug'], 'portal.yml')
    manifest = YAML.safe_load(File.read(path))
    manifest['repositories'] << {'name' => 'alias', 'project' => 'alias', 'branch' => second['slug'], 'default_branch' => 'master'}
    File.write(path, YAML.dump(manifest))
    @engine = engine
    held = acquire(first)
    other = acquire(second, repositories: ['alias'])
    assert_equal held['resources'], other['resources']
    assert_equal 'waiting', other['state']
  end

  def test_legacy_jobs_and_claims_share_exclusion_in_both_directions
    first, second = owner('legacy'), owner('modern')
    job = submit_merge(first)
    held = acquire(second)
    assert_empty @engine.claim(owner_id: first['id'])
    @engine.change_claim(second, held['id'], 'release')
    assert_equal job['id'], @engine.claim(owner_id: first['id']).first['id']
    waiting = acquire(second)
    assert_equal 'waiting', waiting['state']
    @engine.store.transaction do |ledger|
      item = ledger['jobs'][job['id']]
      @engine.transition(ledger, item, 'failed')
    end
    @engine.tick_claims(second)
    assert_equal 'held', @engine.store.claims['claims'][waiting['id']]['state']
  end

  def test_lifecycle_blocks_held_claims_and_cancels_waiters_after_fencing
    first, second = owner('held'), owner('waiting')
    held, waiting = acquire(first), acquire(second)
    assert_raises(WorkspaceCoordination::Error) { @engine.store.require_lifecycle_idle!(@workspace, first['slug']) }
    @engine.store.transaction do |ledger|
      ledger['owners'][second['id']]['fence'] = {'id' => SecureRandom.uuid, 'command' => 'archive', 'completed' => false, 'operationId' => nil}
    end
    assert_equal 'canceled', @engine.store.claims['claims'][waiting['id']]['state']
    @engine.change_claim(first, held['id'], 'release')
    @engine.store.require_lifecycle_idle!(@workspace, first['slug'])
  end

  def test_suspension_and_resource_remapping_prevent_queued_grants
    machines
    first, second = owner('first'), owner('second')
    held = acquire(first, repositories: [], machines: ['first'])
    waiting = acquire(second, repositories: [], machines: ['first'])
    @engine.change_claim(first, held['id'], 'release')
    @engine.store.transaction { |ledger| ledger['suspended'] = true }
    assert_raises(WorkspaceCoordination::Error) { @engine.tick_claims(second) }
    @engine.store.transaction { |ledger| ledger['suspended'] = false }
    @catalog['machines']['first']['id'] = 'fixture:replacement'
    install_catalog
    @engine = engine
    assert_raises(WorkspaceCoordination::Error) { @engine.tick_claims(second) }
    assert_equal 'waiting', @engine.store.claims['claims'][waiting['id']]['state']
    assert_equal 'canceled', @engine.change_claim(second, waiting['id'], 'cancel')['state']
  end

  def test_capability_checks_preserve_state_and_live_machine_mapping
    machines
    person = owner('capability')
    item = acquire(person, repositories: [], machines: ['first'])
    File.unlink(File.join(@package, 'share/dev-workspace/claim-contract.json'))
    assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(@package) }
    File.write(File.join(@package, 'share/dev-workspace/claim-contract.json'), JSON.generate(WorkspaceCoordination::CLAIM_CONTRACT))
    # This capability fixture uses a production-shaped catalog.
    @catalog['repositories']['code']['remote'] = 'git@github.com:example/code.git'
    @catalog['adapters']['fixture']['command'] = File.realpath(RbConfig.ruby)
    install_catalog
    @engine.store.require_claim_capability!(@package)
    @catalog['machines']['first']['id'] = 'fixture:replacement'
    install_catalog
    assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(@package) }
    assert_equal 'held', @engine.store.claims['claims'][item['id']]['state']
  end

  def test_malformed_or_overlapping_state_is_rejected_without_releasing_ownership
    first, second = owner('first'), owner('second')
    held, waiting = acquire(first), acquire(second)
    value = @engine.store.claims
    value['claims'][waiting['id']]['state'] = 'held'
    assert_raises(WorkspaceCoordination::Error) { @engine.store.write_claims(value) }
    value = @engine.store.claims
    value['claims'][held['id']]['request']['argv'] = ['true']
    assert_raises(WorkspaceCoordination::Error) { @engine.store.write_claims(value) }
    assert_equal 'held', @engine.store.claims['claims'][held['id']]['state']
  end

  def test_portal_exposes_claims_and_foreign_machine_ownership_without_command_results
    machines
    first, second = owner('first'), owner('second')
    held = acquire(first, repositories: [], machines: ['first'])
    acquire(second, repositories: [], machines: ['alias-first'])
    status = @engine.status(first['slug'], portal: true)
    assert_equal 'claim', status['reservations'].first['mode']
    assert_equal held['id'], status['claims'].first['id']
    assert_empty status['jobs']
    status = @engine.status(second['slug'], portal: true)
    assert_equal [{'machine' => 'fixture:first', 'state' => 'held', 'portalUrl' => first['portalUrl']}], status['foreignReservations']
  end
  def test_concurrent_acquisition_has_exactly_one_owner
    people = [owner('first'), owner('second')]
    readers, writers = [], []
    people.each do |person|
      reader, writer = IO.pipe
      pid = fork do
        writer.close
        reader.read(1)
        item = acquire(person)
        File.write(File.join(@tmp, person['slug'] + '.json'), JSON.generate(item))
        exit! 0
      end
      reader.close
      readers << pid
      writers << writer
    end
    writers.each { |writer| writer.write('x'); writer.close }
    readers.each { |pid| assert Process.wait2(pid).last.success? }
    assert_equal %w[held waiting], @engine.store.claims['claims'].values.map { |item| item['state'] }.sort
  end

  def test_notification_publication_failure_keeps_grant_and_retries_once
    first, second = owner('first'), owner('second')
    held, waiting = acquire(first), acquire(second)
    @engine.change_claim(first, held['id'], 'release')
    original = @engine.method(:event)
    @engine.define_singleton_method(:event) { |*| raise WorkspaceCoordination::Error, 'fixture outbox unavailable' }
    assert_raises(WorkspaceCoordination::Error) { @engine.tick_claims(second) }
    item = @engine.store.claims['claims'][waiting['id']]
    assert_equal 'held', item['state']
    assert item['notificationPending']
    @engine.define_singleton_method(:event, original)
    @engine.tick_claims(second)
    assert_equal 1, @engine.store.read['outbox'].length
    refute @engine.store.claims['claims'][waiting['id']]['notificationPending']
  end

  def test_retained_preparation_refuses_foreign_claim_before_checkout
    first, second = owner('first'), owner('second')
    acquire(first)
    engine = @engine
    host = DevWorkspaceHost::Host.allocate
    host.define_singleton_method(:claim_workflow_selected?) { true }
    host.define_singleton_method(:coordination_prepare_owned) { |*| raise 'preparation must not run' }
    host.define_singleton_method(:with_current_generation_lock) { |**_options, &block| block.call }
    error = assert_raises(WorkspaceCoordination::Error) do
      host.send(:coordination_prepare, engine, second, {'kind' => 'merge', 'repository' => 'code'})
    end
    assert_equal 75, error.code
    assert_equal 1, @engine.store.claims['claims'].length
  end

  def test_queued_notification_uses_reserved_capacity_after_receipt_limit
    first, second = owner('first'), owner('second')
    held, waiting = acquire(first), acquire(second)
    @engine.store.transaction do |ledger|
      16_385.times { ledger['receipts'][SecureRandom.uuid] = {'digest' => 'sha256:' + 'a' * 64, 'result' => {}} }
    end
    assert_raises(WorkspaceCoordination::Error) { acquire(second) }
    assert_equal 2, @engine.store.claims['claims'].length
    @engine.change_claim(first, held['id'], 'release')
    @engine.tick_claims(second)
    assert_equal 'held', @engine.store.claims['claims'][waiting['id']]['state']
    assert_equal 1, @engine.store.read['outbox'].length
    refute @engine.store.claims['claims'][waiting['id']]['notificationPending']
  end

  def test_waiting_claim_reserves_future_outbox_growth
    first, second = owner('first'), owner('second')
    acquire(first)
    before = @engine.store.recovery_growth(@engine.store.read)
    waiting = acquire(second)
    assert_equal before + WorkspaceCoordination::MAX_RESULT + 64, @engine.store.recovery_growth(@engine.store.read)
    @engine.change_claim(second, waiting['id'], 'cancel')
    assert_equal before, @engine.store.recovery_growth(@engine.store.read)
  end

  def test_retained_job_cli_prepares_approves_and_claims_a_refreshed_snapshot
    person = owner('legacy-refresh')
    job = submit_merge(person)
    checkout = File.join(@tmp, person['slug'])
    File.write(File.join(checkout, person['slug']), 'updated feature')
    git('-C', checkout, 'commit', '-am', 'fixture refresh')
    engine = @engine
    host = DevWorkspaceHost::Host.allocate
    output = StringIO.new
    host.instance_variable_set(:@out, output)
    host.define_singleton_method(:coordination_engine) { |_entry| engine }
    host.define_singleton_method(:claim_workflow_selected?) { true }
    host.define_singleton_method(:with_coordination_session_admission) { |_entry, _slug, &block| block.call }
    host.define_singleton_method(:with_current_generation_lock) { |**_options, &block| block.call }
    invoke = lambda do |action, fields, id = nil, slug = person['slug']|
      path = File.join(@tmp, 'legacy-request.json')
      File.write(path, JSON.generate(fields))
      output.truncate(0)
      output.rewind
      host.send(:coordination_session_command, @entry, [action, slug, id, '--as-is', '--request', path].compact)
      JSON.parse(output.string)
    end
    preparation = request('kind' => 'merge', 'repository' => 'code', 'evidence' => evidence(person))
    assert_raises(WorkspaceCoordination::Error) { invoke.call('prepare', preparation) }
    snap = invoke.call('prepare', preparation.merge('legacyJobId' => job['id']))
    input = request('kind' => 'merge', 'scope' => [{'repository' => 'code', 'target' => 'refs/heads/master'}],
      'source' => {'kind' => 'user_instruction', 'reference' => 'fixture approval', 'quote' => 'integrate code into master'},
      'reviewedSnapshot' => snap['id'])
    other = owner('foreign-approval')
    assert_raises(WorkspaceCoordination::Error) { invoke.call('approve', input, nil, other['slug']) }
    approval = invoke.call('approve', input)
    assert_equal approval, invoke.call('approve', input)
    assert_raises(WorkspaceCoordination::Error) { invoke.call('approve', input.merge('source' => input['source'].merge('quote' => 'changed'))) }
    refreshed = invoke.call('refresh', request('expectedRevision' => job['revision'], 'snapshot' => snap, 'approvalId' => approval['id']), job['id'])
    assert_equal 'waiting_approval', refreshed['state']
    assert_equal job['id'], @engine.claim(owner_id: person['id']).first['id']
    # An approval exception cannot open a new independent workflow.
    assert_raises(WorkspaceCoordination::Error) do
      invoke.call('submit', request('kind' => 'merge', 'dependsOn' => [], 'approvalId' => approval['id'], 'reservationId' => nil, 'snapshot' => snap))
    end
    independent = owner('independent')
    feature(independent)
    independent_snapshot = @engine.prepare_merge(independent, 'code', evidence(independent))
    assert_raises(WorkspaceCoordination::Error) do
      invoke.call('approve', input.merge('requestId' => SecureRandom.uuid, 'reviewedSnapshot' => independent_snapshot['id']), nil, independent['slug'])
    end
  end


end
