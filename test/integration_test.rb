# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require_relative '../libexec/workspace-coordination-execution'
require_relative '../libexec/workspace-integration'
load File.expand_path('../libexec/workspace-host', __dir__)
require_relative 'support/coordination_fixture'

class IntegrationTest < Minitest::Test
  include CoordinationFixture

  def batch(owner, second_dependencies: ['first'])
    feature(owner)
    snapshot = @engine.prepare_merge(owner, 'code', evidence(owner))
    approval_id = approval(owner, snapshot)
    template = {'kind' => 'merge', 'reservationId' => nil, 'snapshot' => snapshot, 'approvalId' => approval_id}
    request('jobs' => [template.merge('key' => 'first', 'dependsOn' => []),
      template.merge('key' => 'second', 'dependsOn' => second_dependencies)])
  end

  def test_batch_admission_binds_dependencies_and_retries_exactly
    first = owner('batch')
    payload = batch(first)
    result = @engine.submit_batch(first, payload)
    jobs = result['jobs']
    assert_equal [jobs[0]['id']], @engine.store.read['jobs'][jobs[1]['id']]['dependsOn']
    assert_equal result, @engine.submit_batch(first, payload)
    assert_equal 2, @engine.store.read['jobs'].length
    error = assert_raises(WorkspaceCoordination::Error) { @engine.submit_batch(first, payload.merge('jobs' => payload['jobs'].reverse)) }
    assert_equal 3, error.code
  end

  def test_invalid_later_job_does_not_publish_the_first
    first = owner('invalid-batch')
    payload = batch(first, second_dependencies: ['future'])
    assert_raises(WorkspaceCoordination::Error) { @engine.submit_batch(first, payload) }
    assert_empty @engine.store.read['jobs']
    refute @engine.store.read['receipts'].key?(payload['requestId'])
  end

  def test_batch_cannot_depend_on_another_owner
    first = owner('first-owner')
    other = owner('other-owner')
    foreign = submit_merge(other)
    payload = batch(first, second_dependencies: [foreign['id']])
    assert_raises(WorkspaceCoordination::Error) { @engine.submit_batch(first, payload) }
    assert_equal [foreign['id']], @engine.store.read['jobs'].keys
  end

  def driver(owner, tracking: false)
    integration = WorkspaceCoordination::Integration.new(@engine, owner, admission: ->(&block) { block.call },
      prepare: ->(fields) { @engine.prepare_merge(owner, fields['repository'], fields['evidence']) }, diagnostics: StringIO.new)
    integration.define_singleton_method(:record_completion) { |_value| } unless tracking
    integration
  end

  def integration_request(owner)
    checkout = feature(owner)
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    review_path = File.join(@workspace, 'work', owner['slug'], 'independent-review.json')
    File.write(review_path, JSON.generate('schema' => 1, 'decision' => 'accepted', 'reviewer' => 'independent-fixture',
      'repository' => 'code', 'target' => 'refs/heads/master', 'base' => @base, 'head' => head, 'migrations' => []))
    review = {'kind' => 'review', 'path' => review_path, 'sha256' => Digest::SHA256.file(review_path).hexdigest}
    request('mode' => 'merge_only', 'repositories' => [{'repository' => 'code', 'target' => 'refs/heads/master',
      'base' => @base, 'head' => head, 'review' => review, 'dependsOn' => []}], 'evidence' => [review],
      'approval' => {'kind' => 'user_instruction', 'reference' => 'fixture instruction', 'quote' => 'merge code into master'})
  end

  def test_driver_resumes_original_jobs_and_exports_compact_results
    first = owner('driver')
    payload = integration_request(first)
    integration = driver(first)
    result = integration.start(payload, wait: false)
    assert_equal 'merging', result['phase']
    jobs = result['jobs'].transform_values { |job| job['id'] }
    assert_equal jobs, integration.start(payload, wait: false)['jobs'].transform_values { |job| job['id'] }
    claimed = @engine.claim(owner_id: first['id'])
    assert_equal 1, claimed.length
    # Real merge/push with only a disposable fixture repository.
    job = claimed.first
    request_path = @engine.attempt_path(job['attempt'], 'request.json')
    WorkspaceCoordination.atomic(request_path, @engine.worker_request(job, first))
    WorkspaceCoordination::Worker.new(request_path, fixtures: true).run
    @engine.reconcile_attempts
    pending = @engine.store.read['outbox']
    refute_empty pending
    finished = integration.resume(payload['requestId'])
    assert_equal 'complete', finished['phase']
    assert_equal pending, @engine.store.read['outbox']
    assert_equal 1, @engine.store.read['jobs'].length
    assert File.file?(File.join(@workspace, 'work', first['slug'], 'integration', payload['requestId'], 'result.json'))
  end

  def test_driver_retains_step_identity_when_a_reply_is_lost
    first = owner('lost-reply')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    journal = integration.read(payload['requestId'])
    submitted = journal['steps']['submit-batch']
    submitted.delete('result')
    journal['phase'] = 'submit'
    integration.save(journal)
    integration.resume(payload['requestId'])
    assert_equal 1, @engine.store.read['jobs'].length
    assert_equal submitted['requestId'], integration.read(payload['requestId'])['steps']['submit-batch']['requestId']
  end

  def test_approval_reply_loss_recovers_the_original_receipt_before_submission
    first = owner('lost-approval-reply')
    payload = integration_request(first)
    integration = driver(first)
    original = @engine.method(:mutate)
    lost = false
    @engine.define_singleton_method(:mutate) do |*args, **keywords|
      result = original.call(*args, **keywords)
      if args[1] == 'approve' && !lost
        lost = true
        raise WorkspaceCoordination::Error.new('fixture lost approval response', 75)
      end
      result
    end
    assert_raises(WorkspaceCoordination::Error) { integration.start(payload, wait: false) }
    journal = integration.read(payload['requestId'])
    assert journal['operations']['code']['snapshot']
    refute journal['operations']['code']['approvalId']
    approval_request = journal['steps']['approve-code']['requestId']
    integration.resume(payload['requestId'])
    assert_equal 1, @engine.store.read['approvals'].length
    assert_equal approval_request, integration.read(payload['requestId'])['steps']['approve-code']['requestId']
    assert_equal 1, @engine.claim(owner_id: first['id']).length
  end

  def test_retained_tracking_claim_covers_workspace_without_feature_registration
    first, other = owner('tracking-owner'), owner('claim-holder')
    git('init', '--initial-branch=master', @workspace)
    git('-C', @workspace, 'remote', 'add', 'origin', @remote)
    @catalog['repositories']['workspace'] = {'remote' => @remote}
    install_catalog
    @engine = engine
    File.write(File.join(@package, 'share/dev-workspace/claim-contract.json'), JSON.generate(WorkspaceCoordination::CLAIM_CONTRACT))
    held = @engine.acquire_claim(other, request('repositories' => ['code'], 'machines' => [], 'label' => 'same canonical remote'))
    integration = driver(first, tracking: true)
    called = false
    integration.define_singleton_method(:record_completion_owned) { |_value| called = true }
    assert_raises(WorkspaceCoordination::Error) { integration.record_completion({}) }
    refute called
    @engine.change_claim(other, held['id'], 'release')
    integration.record_completion({})
    assert called
    assert_empty @engine.store.held_claim_resources
  end

  def test_retained_refresh_cannot_mutate_before_foreign_repository_claim_releases
    first, other = owner('retained'), owner('claim-holder')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    id = integration.read(payload['requestId'])['jobs']['merge:code']
    @engine.store.transaction { |data| @engine.transition(data, data['jobs'].fetch(id), 'needs_refresh') }
    upstream = File.join(@tmp, 'seed')
    File.write(File.join(upstream, 'upstream'), 'new target baseline')
    git('-C', upstream, 'add', 'upstream')
    git('-C', upstream, 'commit', '-m', 'fixture new baseline')
    git('-C', upstream, 'push', 'origin', 'master')
    File.write(File.join(@package, 'share/dev-workspace/claim-contract.json'), JSON.generate(WorkspaceCoordination::CLAIM_CONTRACT))
    held = @engine.acquire_claim(other, request('repositories' => ['code'], 'machines' => [], 'label' => 'ordinary Git work'))
    before = git('-C', @bare, 'show-ref')
    capture = @engine.method(:capture)
    mutations = []
    @engine.define_singleton_method(:capture) do |*args, **options|
      mutations << args if %w[fetch checkout worktree rebase update-ref push].any? { |word| args.include?(word) }
      capture.call(*args, **options)
    end
    error = assert_raises(WorkspaceCoordination::Error) { integration.resume(payload['requestId']) }
    assert_equal 75, error.code
    assert_empty mutations
    assert_equal before, git('-C', @bare, 'show-ref')
    @engine.change_claim(other, held['id'], 'release')
    # Stop after entering the now-owned fetch; the ordinary full refresh has
    # separate certification tests and requires its canonical feature layout.
    @engine.define_singleton_method(:capture) do |*args, **options|
      throw :owned_fetch if args.include?('fetch')
      capture.call(*args, **options)
    end
    reached = false
    catch(:owned_fetch) { integration.resume(payload['requestId']); reached = true }
    refute reached
    assert_empty @engine.store.held_claim_resources
  end

  def test_refresh_reply_loss_recovers_snapshot_and_original_mutation
    first = owner('lost-refresh-reply')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    id = integration.read(payload['requestId'])['jobs']['merge:code']
    @engine.store.transaction do |data|
      @engine.transition(data, data['jobs'].fetch(id), 'needs_refresh')
    end
    original = @engine.method(:mutate)
    lost = false
    @engine.define_singleton_method(:mutate) do |*args, **keywords|
      result = original.call(*args, **keywords)
      if args[1] == 'refresh' && !lost
        lost = true
        raise WorkspaceCoordination::Error.new('fixture lost refresh response', 75)
      end
      result
    end
    assert_raises(WorkspaceCoordination::Error) { integration.resume(payload['requestId']) }
    journal = integration.read(payload['requestId'])
    assert journal['pendingRefresh']['snapshot']
    refute_equal 'needs_refresh', @engine.store.read['jobs'][id]['state']
    integration.resume(payload['requestId'])
    recovered = integration.read(payload['requestId'])
    refute recovered.key?('pendingRefresh')
    assert_equal @engine.store.read['jobs'][id]['snapshot'], recovered['operations']['code']['snapshot']
    assert_equal 1, @engine.store.read['jobs'][id]['history'].length
  end

  def test_edit_during_checks_cannot_receive_review_approval
    first = owner('edited-during-checks')
    payload = integration_request(first)
    integration = driver(first)
    checkout = File.join(@tmp, first['slug'])
    integration.define_singleton_method(:verify_checks) do |_value, _name|
      File.write(File.join(checkout, 'unreviewed'), 'new behavior')
      engine.capture('git', '-C', checkout, 'add', 'unreviewed')
      engine.capture('git', '-C', checkout, 'commit', '-m', 'fixture unreviewed behavior')
    end
    error = assert_raises(WorkspaceCoordination::Error) { integration.start(payload, wait: false) }
    assert_equal 4, error.code
    assert_empty @engine.store.read['approvals']
    assert_empty @engine.store.read['jobs']
  end

  def test_rejected_refresh_snapshot_stays_rejected_on_resume
    first = owner('rejected-refresh-resume')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    id = integration.read(payload['requestId'])['jobs']['merge:code']
    @engine.store.transaction { |data| @engine.transition(data, data['jobs'][id], 'needs_refresh') }
    checkout = File.join(@tmp, first['slug'])
    original = integration.instance_variable_get(:@prepare)
    integration.instance_variable_set(:@prepare, lambda do |fields|
      File.write(File.join(checkout, 'unreviewed'), 'new behavior')
      git('-C', checkout, 'add', 'unreviewed')
      git('-C', checkout, 'commit', '-m', 'fixture unreviewed behavior')
      original.call(fields)
    end)
    2.times do
      error = assert_raises(WorkspaceCoordination::Error) { integration.resume(payload['requestId']) }
      assert_equal 4, error.code
      assert_equal 1, @engine.store.read['approvals'].length
      assert_equal 'needs_refresh', @engine.store.read['jobs'][id]['state']
    end
    refute integration.read(payload['requestId'])['pendingRefresh']['snapshot']
  end

  def test_deploy_first_prepares_merges_in_the_selected_generation
    first = owner('deployed-generation')
    payload = integration_request(first).merge('mode' => 'deploy_first', 'deployment' => {})
    integration = driver(first)
    integration.define_singleton_method(:deploy) do |value|
      value['phase'] = 'deploying'
    end
    integration.start(payload, wait: false)
    predecessor = integration.read(payload['requestId'])['operations']['code']['snapshot']['preparedGeneration']
    replacement_link = @profile + '.next'
    File.symlink(@package, replacement_link)
    File.rename(replacement_link, @profile)
    integration.define_singleton_method(:settled) { |_value, _names| true }
    result = integration.resume(payload['requestId'])
    assert_equal 'merging', result['phase']
    snapshot = integration.read(payload['requestId'])['operations']['code']['snapshot']
    refute_equal predecessor['profileToken'], snapshot['preparedGeneration']['profileToken']
    assert_equal DevWorkspaceProfileIdentity.token(@profile), snapshot['preparedGeneration']['profileToken']
    assert_equal 1, @engine.claim(owner_id: first['id']).length
  end

  def test_export_refuses_to_recreate_archived_tracking
    first = owner('archived-export')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    journal = integration.read(payload['requestId'])
    root = File.join(@workspace, 'work', first['slug'])
    FileUtils.mkdir_p(File.join(@workspace, 'archive'))
    File.rename(root, File.join(@workspace, 'archive', first['slug']))
    assert_raises(Errno::ENOENT) { integration.export(journal) }
    refute File.exist?(root)
  end

  def test_separate_run_exports_keep_matching_summary_and_result
    first = owner('multiple-exports')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    initial = integration.read(payload['requestId'])
    integration.export(initial)
    later = Marshal.load(Marshal.dump(initial))
    later['id'] = SecureRandom.uuid
    integration.save(later)
    integration.export(later)
    [initial, later].each do |journal|
      directory = File.join(@workspace, 'work', first['slug'], 'integration', journal['id'])
      assert_equal journal['id'], JSON.parse(File.read(File.join(directory, 'result.json')))['id']
      assert_includes File.read(File.join(directory, 'summary.md')), journal['id']
    end
  end

  def test_completed_recipe_with_lost_reply_recovers_through_read_only_verification
    first = owner('lost-recipe-reply')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    journal = integration.read(payload['requestId'])
    item = payload['repositories'].first.merge('recipe' => {'name' => 'fixture', 'parameters' =>
      {'repository' => 'code', 'inputPath' => ['runtime']}})
    integration.define_singleton_method(:recipe_heads) { |_value, _item, _path| {'code' => item['head']} }
    integration.instance_variable_get(:@catalog).define_singleton_method(:recipe) { |_name| {'command' => 'fixture-recipe'} }
    checkout = File.join(@workspace, 'worktrees', first['slug'], 'code')
    FileUtils.mkdir_p(File.dirname(checkout))
    git('-C', @bare, 'worktree', 'move', File.join(@tmp, first['slug']), checkout)
    original = @engine.method(:capture)
    actions = []
    @engine.define_singleton_method(:capture) do |*args, **options|
      next original.call(*args, **options) unless args.first == 'fixture-recipe'
      request = JSON.parse(options.fetch(:input))
      actions << request['action']
      source = request.fetch('source')
      if request['action'] == 'refresh'
        File.write(File.join(source, 'generated'), 'fixture pin')
        original.call('git', '-C', source, 'add', 'generated')
        original.call('git', '-C', source, 'commit', '-m', 'fixture generated pin')
        raise WorkspaceCoordination::Error.new('fixture lost recipe response', 75)
      end
      head = original.call('git', '-C', source, 'rev-parse', 'HEAD').strip
      JSON.generate('schema' => 1, 'verified' => true, 'head' => head, 'proof' => {'relation' => 'generated_pin'})
    end
    path, _, _, registration = @engine.repository(first, 'code')
    assert_raises(WorkspaceCoordination::Error) do
      integration.refresh_recipe(journal, item, path, registration, @base, item['head'])
    end
    integration.refresh_recipe(journal, item, path, registration, @base, item['head'])
    assert_equal %w[refresh verify], actions
    generated = git('-C', checkout, 'rev-parse', 'HEAD').strip
    assert_equal generated, git('-C', @bare, 'ls-remote', 'origin', "refs/heads/#{first['slug']}").split.first
    assert journal['steps'].values.any? { |entry| entry.dig('result', 'head') == generated }
  end

  def test_sensitive_completion_keeps_tracking_local
    first = owner('sensitive-completion')
    payload = integration_request(first)
    integration = driver(first, tracking: true)
    integration.start(payload, wait: false)
    root = File.join(@workspace, 'work', first['slug'])
    manifest_path = File.join(root, 'portal.yml')
    manifest = YAML.safe_load(File.read(manifest_path))
    manifest['sensitive'] = true
    File.write(manifest_path, YAML.dump(manifest))
    original = @engine.method(:capture)
    artifacts = []
    @engine.define_singleton_method(:capture) do |*args, **options|
      if args.first == File.join(package, 'bin/dev-session')
        artifacts << args[4]
        next ''
      end
      raise 'sensitive completion must not write Git' if %w[add commit push fetch merge].any? { |word| args.include?(word) }
      original.call(*args, **options)
    end
    journal = integration.read(payload['requestId'])
    integration.record_completion(journal)
    assert_equal({'local' => true}, journal['steps']['tracking-commit']['result'])
    assert_includes File.read(File.join(root, 'state.md')), payload['requestId']
    assert_equal %w[summary.md result.json].map { |file| File.join('integration', payload['requestId'], file) }, artifacts
  end

  def test_tracking_completion_does_not_wait_for_informational_delivery
    first = owner('notification-pending')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    @engine.claim(owner_id: first['id'])
    journal = integration.read(payload['requestId'])
    @engine.store.transaction do |data|
      data['jobs'].each_value { |job| job['state'] = 'succeeded' }
    end
    pending = @engine.store.read['outbox']
    refute_empty pending
    root = File.join(@workspace, 'work', first['slug'])
    manifest = YAML.safe_load(File.read(File.join(root, 'portal.yml')))
    integration.update_tracking_lifecycle(journal, manifest)
    assert_match /\Alifecycle: complete\n/, File.read(File.join(root, 'state.md')).delete_prefix("---\n")
    assert_equal pending, @engine.store.read['outbox']
    assert_raises(WorkspaceCoordination::Error) { @engine.owner(first['slug']) }

    # Exercise the scheduler's per-owner path and its launched worker command,
    # with only process creation and the native notification transport stubbed.
    entry = @entry.merge('root' => @workspace)
    host = Object.new.extend(DevWorkspaceHost::WorkspaceCoordinationHost)
    engine, package, profile = @engine, @package, @profile
    admissions = []
    host.instance_variable_set(:@profile, profile)
    host.define_singleton_method(:registry) { Struct.new(:entries).new([entry]) }
    host.define_singleton_method(:package_root) { package }
    host.define_singleton_method(:coordination_engine) { |_entry| engine }
    host.define_singleton_method(:with_coordination_session_admission) do |_entry, slug, **_options, &block|
      admissions << slug
      block.call
    end
    @engine.define_singleton_method(:capture) do |*argv, **_options|
      next "inactive\n" if argv.first == 'systemctl'
      host.coordination_notifications_command(['--request', argv.last]) if argv.first == 'systemd-run'
      ''
    end
    @engine.define_singleton_method(:notification_call) do |recipient, _event, action|
      action == 'prepare' ? {'options' => {'model' => 'retained'}} : {'receipt' => {'threadId' => recipient['thread']}}
    end
    before_owners = @engine.store.read['owners']
    host.coordinate_owner(entry, @engine, first)
    assert_equal [first['slug'], first['slug']], admissions
    assert_equal before_owners, @engine.store.read['owners']
    assert @engine.store.read['outbox'].values.all? { |event| event['state'] == 'delivered' }
    assert_raises(WorkspaceCoordination::Error) { @engine.owner(first['slug']) }
  end

  def test_read_only_status_never_creates_dates_or_changes_ledger
    first = owner('read-status')
    payload = integration_request(first)
    integration = driver(first)
    integration.start(payload, wait: false)
    before = File.binread(File.join(@engine.store.root, 'ledger.json'))
    integration.status(payload['requestId'])
    assert_equal before, File.binread(File.join(@engine.store.root, 'ledger.json'))
  end

  def test_notification_dispatch_launches_a_separate_owner_worker
    first = owner('notification-worker')
    submit_merge(first)
    @engine.claim(owner_id: first['id'])
    calls = []
    @engine.define_singleton_method(:capture) { |*argv, **_options| calls << argv; argv.first == 'systemctl' ? "inactive\n" : '' }
    @engine.dispatch_notifications(first)
    launch = calls.find { |argv| argv.first == 'systemd-run' }
    assert launch
    assert_includes launch, 'coordination-notifications'
    request = WorkspaceCoordination.private_read(File.join(@engine.store.root, 'notification-workers', "#{first['id']}.json"))
    assert_equal first['identity'], request['identity']
    assert_equal @package, request['package']
  end

  def test_pending_run_refuses_a_predecessor_without_integration_capability
    first = owner('capability')
    payload = integration_request(first)
    driver(first).start(payload, wait: false)
    # A catalog without physical targets is enough to exercise the capability
    # fence; the original fixture's local remote is intentionally test-only.
    WorkspaceCoordination.atomic(File.join(@package, 'share/dev-workspace/coordination-contract.json'), WorkspaceCoordination::CONTRACT)
    File.write(File.join(@package, 'share/dev-workspace/coordination.json'), JSON.generate('schema' => 1, 'adapters' => {}, 'machines' => {}, 'repositories' => {}, 'profileMachine' => nil))
    error = assert_raises(WorkspaceCoordination::Error) { @engine.store.require_compatible!(@package) }
    assert_equal 5, error.code
    assert_includes error.message, 'integration journals'
  end

  def test_pending_manual_run_accepts_legacy_and_dual_journal_packages
    first = owner('manual-upgrade')
    payload = integration_request(first)
    driver(first).start(payload, wait: false)
    shared = File.join(@package, 'share/dev-workspace')
    WorkspaceCoordination.atomic(File.join(shared, 'coordination-contract.json'), WorkspaceCoordination::CONTRACT)
    File.write(File.join(shared, 'coordination.json'), JSON.generate('schema' => 1, 'adapters' => {}, 'machines' => {}, 'repositories' => {}, 'profileMachine' => nil))
    File.write(File.join(shared, 'package.json'), JSON.generate('integration' => {'journalSchema' => 1, 'certificationSchema' => 1}))
    integration_path = File.join(@engine.store.root, 'integrations', "#{payload['requestId']}.json")
    before = File.binread(integration_path)
    @engine.store.require_compatible!(@package)
    FileUtils.cp(File.expand_path('../nix/integration-contract.json', __dir__), File.join(shared, 'integration-contract.json'))
    @engine.store.require_compatible!(@package)
    assert_equal before, File.binread(integration_path)
    assert_equal 1, JSON.parse(before).fetch('schema')
    refute_equal 'complete', JSON.parse(before).fetch('phase')
  end
  def deliver_background_notifications(owner)
    @engine.define_singleton_method(:notification_call) do |recipient, _event, action|
      action == 'prepare' ? {'options' => {'model' => 'retained'}} : {'receipt' => {'threadId' => recipient['thread']}}
    end
    @engine.deliver_notifications(owner_id: owner['id'])
  end

  def test_background_admission_returns_before_preparation_and_reserves_lifecycle
    first = owner('background-admission')
    integration = driver(first)
    payload = integration_request(first)
    result = integration.start(payload, background: true)
    assert_equal 2, result['schema']
    assert_equal 'prepare', result['phase']
    assert_equal 'runnable', result.dig('continuation', 'state')
    assert_empty @engine.store.read['jobs']
    assert @engine.store.session_blockers(@workspace, first['slug']).any? { |text| text.include?('background integration') }
    assert_equal result, integration.start(payload, background: true)
    assert_raises(WorkspaceCoordination::Error) { integration.start(payload, wait: false) }
  end

  def test_scheduler_dispatches_background_preparation_without_pending_jobs
    first = owner('background-scheduler')
    payload = integration_request(first)
    driver(first).start(payload, background: true)
    assert_empty @engine.store.read['jobs']
    launches = []
    @engine.define_singleton_method(:capture) do |*argv, **_options|
      next "inactive\n" if argv.first == 'systemctl'
      launches << argv if argv.first == 'systemd-run'
      ''
    end
    host = Object.new.extend(DevWorkspaceHost::WorkspaceCoordinationHost)
    host.define_singleton_method(:with_coordination_session_admission) { |*_args, **_options, &block| block.call }
    host.coordinate_owner(@entry, @engine, first)
    assert_equal 1, launches.length
    assert_includes launches.first, 'coordination-integration'
  end

  def test_background_run_continues_without_an_agent_and_finishes_before_delivery
    first = owner('background-completion')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    # Claimed/running/completed intermediate steps do not wake the model.
    claimed = @engine.claim(owner_id: first['id']).first
    assert_empty @engine.store.read['outbox']
    request_path = @engine.attempt_path(claimed['attempt'], 'request.json')
    WorkspaceCoordination.atomic(request_path, @engine.worker_request(claimed, first))
    WorkspaceCoordination::Worker.new(request_path, fixtures: true).run
    @engine.reconcile_attempts
    assert_empty @engine.store.read['outbox']
    # A new process/driver resumes the original run and original job identities.
    fresh = driver(first)
    result = fresh.run_background(payload['requestId'])
    assert_equal 'complete', result['phase']
    assert_equal 1, @engine.store.read['outbox'].length
    assert @engine.store.read['outbox'].values.all? { |event| event['state'] == 'pending' }
    fresh.run_background(payload['requestId'])
    assert_equal 1, @engine.store.read['outbox'].length
    deliver_background_notifications(first)
    assert_equal 'complete', driver(first).run_background(payload['requestId'])['phase']
    assert_equal 1, @engine.store.read['jobs'].length
    assert_equal 'complete', integration.read(payload['requestId']).dig('continuation', 'state')
    # Interruption after phase publication but before continuation publication
    # must still launch recovery without repeating any completed operation.
    interrupted = integration.read(payload['requestId'])
    interrupted['continuation']['state'] = 'runnable'
    integration.save(interrupted)
    jobs_before = @engine.store.read['jobs']
    launches = []
    engine = @engine
    @engine.define_singleton_method(:capture) do |*argv, **_options|
      next "inactive\n" if argv.first == 'systemctl'
      if argv.first == 'systemd-run'
        launches << argv
        WorkspaceCoordination::Integration.new(engine, first, admission: ->(&block) { block.call },
          prepare: ->(*) { raise 'completed operations must not prepare again' }, diagnostics: StringIO.new).run_background(payload['requestId'])
      end
      ''
    end
    host = Object.new.extend(DevWorkspaceHost::WorkspaceCoordinationHost)
    host.define_singleton_method(:with_coordination_session_admission) { |*_args, **_options, &block| block.call }
    host.coordinate_owner(@entry, @engine, first)
    assert_equal 1, launches.length
    assert_includes launches.first, 'coordination-integration'
    assert_equal 'complete', integration.read(payload['requestId']).dig('continuation', 'state')
    assert_equal jobs_before, @engine.store.read['jobs']
  end

  def test_background_event_routing_survives_lost_submission_reply
    first = owner('background-routing')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    value = integration.read(payload['requestId'])
    value['jobs'] = {}
    value['steps']['submit-batch'].delete('result')
    integration.save(value)
    @engine.claim(owner_id: first['id'])
    assert_empty @engine.store.read['outbox']
    assert_equal payload['requestId'], @engine.store.background_integration(@engine.store.read, first['id'], @engine.store.read['jobs'].keys.first)['id']
  end

  def test_background_replayed_submission_observes_completed_jobs_without_another_ledger_change
    first = owner('background-submission-replay')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    claimed = @engine.claim(owner_id: first['id']).first
    request_path = @engine.attempt_path(claimed['attempt'], 'request.json')
    WorkspaceCoordination.atomic(request_path, @engine.worker_request(claimed, first))
    WorkspaceCoordination::Worker.new(request_path, fixtures: true).run
    @engine.reconcile_attempts

    # The worker died after batch admission but before saving its reply/phase.
    value = integration.read(payload['requestId'])
    value['phase'] = 'submit'
    value['jobs'] = {}
    value['steps']['submit-batch'].delete('result')
    value['continuation']['state'] = 'runnable'
    integration.save(value)
    revision = @engine.store.read['revision']
    result = driver(first).run_background(payload['requestId'])
    assert_equal revision, @engine.store.read['revision']
    assert_equal 'merging', result['phase']
    assert_equal 'runnable', result.dig('continuation', 'state')

    calls = []
    original_capture = @engine.method(:capture)
    @engine.define_singleton_method(:capture) do |*argv, **options|
      if argv.first == 'systemctl'
        "inactive\n"
      elsif argv.first == 'systemd-run'
        calls << argv
        ''
      else
        original_capture.call(*argv, **options)
      end
    end
    @engine.dispatch_integrations(first)
    assert_equal 1, calls.length
    assert_equal 'complete', driver(first).run_background(payload['requestId'])['phase']
    assert_equal [claimed['id']], @engine.store.read['jobs'].keys
    assert_equal 1, @engine.store.read['outbox'].length
    deliver_background_notifications(first)
    assert_equal 'complete', driver(first).run_background(payload['requestId'])['phase']
    assert_equal 1, @engine.store.read['outbox'].length
  end

  def test_background_replayed_refresh_observes_completed_jobs_without_another_ledger_change
    first = owner('background-refresh-replay')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    id = integration.read(payload['requestId'])['jobs']['merge:code']
    @engine.store.transaction { |data| @engine.transition(data, data['jobs'].fetch(id), 'needs_refresh') }
    original = @engine.method(:mutate)
    lost = false
    @engine.define_singleton_method(:mutate) do |*args, **keywords|
      result = original.call(*args, **keywords)
      if args[1] == 'refresh' && !lost
        lost = true
        raise 'fixture crash after committed refresh'
      end
      result
    end
    assert_raises(RuntimeError) { integration.run_background(payload['requestId']) }
    assert integration.read(payload['requestId']).key?('pendingRefresh')
    claimed = @engine.claim(owner_id: first['id']).first
    request_path = @engine.attempt_path(claimed['attempt'], 'request.json')
    WorkspaceCoordination.atomic(request_path, @engine.worker_request(claimed, first))
    WorkspaceCoordination::Worker.new(request_path, fixtures: true).run
    @engine.reconcile_attempts
    revision = @engine.store.read['revision']
    result = driver(first).run_background(payload['requestId'])
    assert_equal revision, @engine.store.read['revision']
    assert_equal 'merging', result['phase']
    assert_equal 'runnable', result.dig('continuation', 'state')
    refute integration.read(payload['requestId']).key?('pendingRefresh')
    assert_equal 'complete', driver(first).run_background(payload['requestId'])['phase']
    deliver_background_notifications(first)
    assert_equal 'complete', driver(first).run_background(payload['requestId'])['phase']
    assert_equal [id], @engine.store.read['jobs'].keys
    assert_equal 1, @engine.store.read['jobs'][id]['history'].length
    assert_equal 1, @engine.store.read['outbox'].length
  end

  def test_background_pause_prevents_admission_and_explicit_resume_reuses_the_run
    first = owner('background-pause')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    assert_equal 'paused', integration.pause(payload['requestId']).dig('continuation', 'state')
    assert_equal 'paused', integration.run_background(payload['requestId']).dig('continuation', 'state')
    assert_empty @engine.store.read['jobs']
    integration.arm_background(payload['requestId'])
    integration.run_background(payload['requestId'])
    assert_equal 1, @engine.store.read['jobs'].length
    assert_equal 1, integration.read(payload['requestId']).dig('continuation', 'generation')
  end

  def test_background_error_is_actionable_once_and_crash_retry_does_not_repeat_work
    first = owner('background-error')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    attempts = 0
    integration.define_singleton_method(:advance) do |_value|
      attempts += 1
      raise WorkspaceCoordination::Error.new('changed deployment artifact needs preparation', 3)
    end
    result = integration.run_background(payload['requestId'])
    assert_equal 'needs_attention', result.dig('continuation', 'state')
    assert_equal 1, @engine.store.read['outbox'].length
    integration.run_background(payload['requestId'])
    assert_equal 1, attempts
    assert_equal 1, @engine.store.read['outbox'].length
    # The error-state publication can precede a lost notification transaction.
    @engine.store.transaction { |data| data['outbox'].clear }
    integration.run_background(payload['requestId'])
    assert_equal 1, attempts
    assert_equal 1, @engine.store.read['outbox'].length
  end

  def test_background_fencing_before_first_job_prevents_preparation
    first = owner('background-fence')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    @engine.store.fence(@workspace, first['slug'], 'archive')
    assert_equal 'held', integration.run_background(payload['requestId']).dig('continuation', 'state')
    assert_empty @engine.store.read['jobs']
    assert_empty @engine.store.read['outbox']
  end

  def test_background_failed_command_is_actionable_even_with_exit_code_75
    first = owner('background-command-error')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    attempts = 0
    integration.define_singleton_method(:advance) do |_value|
      attempts += 1
      engine.capture('sh', '-c', 'echo failed-check >&2; exit 1')
    end
    result = integration.run_background(payload['requestId'])
    assert_equal 'needs_attention', result.dig('continuation', 'state')
    assert_equal 75, result.dig('reason', 'code')
    assert_includes result.dig('reason', 'message'), 'failed-check'
    integration.run_background(payload['requestId'])
    assert_equal 1, attempts
    assert_equal 1, @engine.store.read['outbox'].length
  end

  def test_background_host_preparation_failure_is_actionable_once
    first = owner('background-host-error')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    seed = File.join(@tmp, 'seed')
    @engine.define_singleton_method(:repository) { |_owner, _name| [seed, nil, nil, {'branch' => 'master'}] }
    @engine.define_singleton_method(:retain_commit) { |_path, _head| seed }
    host = Object.new.extend(DevWorkspaceHost::WorkspaceCoordinationHost)
    host.define_singleton_method(:claim_workflow_selected?) { false }
    host.define_singleton_method(:with_current_generation_lock) { |**_options, &block| block.call }
    attempts = 0
    host.define_singleton_method(:prepare_profile) do |_argv|
      attempts += 1
      raise DevWorkspaceHost::Error, 'candidate profile validation failed'
    end
    engine = @engine
    integration.define_singleton_method(:advance) do |_value|
      host.coordination_prepare(engine, first, {'kind' => 'profile_activate'})
    end
    result = integration.run_background(payload['requestId'])
    assert_equal 'needs_attention', result.dig('continuation', 'state')
    assert_equal 'candidate profile validation failed', result.dig('reason', 'message')
    integration.run_background(payload['requestId'])
    assert_equal 1, attempts
    assert_equal 1, @engine.store.read['outbox'].length
  end

  def test_background_lifecycle_admission_contention_waits_without_notifying
    first = owner('background-admission-busy')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    lock_path = File.join(@tmp, 'lifecycle.lock')
    File.open(lock_path, 'w+', 0o600) do |writer|
      writer.flock(File::LOCK_EX)
      integration.define_singleton_method(:advance) do |_value|
        File.open(lock_path, 'r+') { |reader| WorkspaceCoordination::Admission.acquire(reader, timeout: 0) }
      end
      result = integration.run_background(payload['requestId'])
      assert_equal 'waiting', result.dig('continuation', 'state')
      assert_nil result.dig('continuation', 'observedRevision')
      assert_empty @engine.store.read['outbox']
    end
  end

  def test_background_waiting_is_not_redispatched_without_a_ledger_change
    first = owner('background-dispatch')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    calls = []
    @engine.define_singleton_method(:capture) { |*argv, **_options| calls << argv; argv.first == 'systemctl' ? "inactive\n" : '' }
    @engine.dispatch_integrations(first)
    # Submission changed the ledger after the worker's initial observation.
    assert calls.any? { |call| call.first == 'systemd-run' && call.include?('coordination-integration') }
    integration.run_background(payload['requestId'])
    calls.clear
    @engine.dispatch_integrations(first)
    assert_empty calls
    @engine.claim(owner_id: first['id'])
    @engine.dispatch_integrations(first)
    assert calls.any? { |call| call.first == 'systemd-run' }
  end

  def test_background_state_refuses_predecessor_even_after_completion
    first = owner('background-capability')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    WorkspaceCoordination.atomic(File.join(@package, 'share/dev-workspace/coordination-contract.json'), WorkspaceCoordination::CONTRACT)
    File.write(File.join(@package, 'share/dev-workspace/coordination.json'), JSON.generate('schema' => 1, 'adapters' => {}, 'machines' => {}, 'repositories' => {}, 'profileMachine' => nil))
    metadata = File.join(@package, 'share/dev-workspace/package.json')
    File.write(metadata, JSON.generate('integration' => {'journalSchema' => 1, 'certificationSchema' => 1}))
    assert_raises(WorkspaceCoordination::Error) { @engine.store.require_compatible!(@package) }
    contract = File.join(@package, 'share/dev-workspace/integration-contract.json')
    FileUtils.cp(File.expand_path('../nix/integration-contract.json', __dir__), contract)
    @engine.store.require_compatible!(@package)
    File.write(contract, JSON.generate(WorkspaceCoordination::INTEGRATION_CONTRACT.merge('journalSchemas' => [1, 2, 3])))
    @engine.store.require_compatible!(@package)
    FileUtils.cp(File.expand_path('../nix/integration-contract.json', __dir__), contract)
    value = integration.read(payload['requestId'])
    value['phase'] = 'complete'
    value['continuation']['state'] = 'complete'
    integration.save(value)
    @engine.store.require_compatible!(@package)
    File.unlink(contract)
    assert_raises(WorkspaceCoordination::Error) { @engine.store.require_compatible!(@package) }
    [nil, {'schema' => 1, 'journalSchemas' => [1, 3], 'certificationSchema' => 1},
      WorkspaceCoordination::INTEGRATION_CONTRACT.merge('journalSchemas' => [1, 2, 2]),
      WorkspaceCoordination::INTEGRATION_CONTRACT.merge('journalSchemas' => [1, '2']),
      WorkspaceCoordination::INTEGRATION_CONTRACT.merge('journalSchemas' => [2]),
      WorkspaceCoordination::INTEGRATION_CONTRACT.merge('schema' => 2),
      WorkspaceCoordination::INTEGRATION_CONTRACT.merge('certificationSchema' => 2),
      WorkspaceCoordination::INTEGRATION_CONTRACT.merge('unexpected' => true)].each do |invalid|
      File.write(contract, JSON.generate(invalid))
      error = assert_raises(WorkspaceCoordination::Error) { @engine.store.require_compatible!(@package) }
      assert_includes error.message, 'integration journals'
    end
  end

  def test_background_pause_also_prevents_claiming_already_submitted_jobs
    first = owner('background-queued-pause')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    integration.pause(payload['requestId'])
    assert_empty @engine.claim(owner_id: first['id'])
    assert_equal 'paused', @engine.status(first['slug'])['integrations'].first.dig('continuation', 'state')
    assert_equal 'integration_paused', @engine.store.read['jobs'].values.first.dig('reason', 'code')
    integration.arm_background(payload['requestId'])
    assert_equal 1, @engine.claim(owner_id: first['id']).length
  end

  def test_background_handover_does_not_turn_generation_change_into_agent_work
    first = owner('background-generation')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    successor = File.join(@tmp, 'successor')
    FileUtils.mkdir_p(successor)
    File.unlink(@profile)
    File.symlink(successor, @profile)
    @engine.instance_variable_set(:@fixtures, false)
    assert_equal 'runnable', integration.run_background(payload['requestId']).dig('continuation', 'state')
    assert_empty @engine.store.read['outbox']
    assert_empty @engine.store.read['jobs']
  end

  def test_background_handover_checks_profile_token_when_package_is_unchanged
    first = owner('background-token-generation')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    @engine.instance_variable_set(:@fixtures, false)
    assert @engine.current_generation?
    replacement = File.join(@tmp, 'replacement-profile')
    File.symlink(@package, replacement)
    File.rename(replacement, @profile)
    assert_equal @engine.package, File.realpath(@profile)
    refute @engine.current_generation?
    assert_equal 'runnable', integration.run_background(payload['requestId']).dig('continuation', 'state')
    assert_empty @engine.store.read['outbox']
    assert_empty @engine.store.read['jobs']
  end

  def test_background_pause_acknowledgment_serializes_with_claim_admission
    first = owner('background-pause-race')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    checked, proceed, pause_started = Queue.new, Queue.new, Queue.new
    original = @engine.store.method(:background_stopped?)
    armed = true
    @engine.store.define_singleton_method(:background_stopped?) do |run|
      result = original.call(run)
      if run && armed
        armed = false
        checked << true
        proceed.pop
      end
      result
    end
    claimer = Thread.new { @engine.claim(owner_id: first['id']) }
    checked.pop
    pause_driver = WorkspaceCoordination::Integration.new(@engine, first,
      admission: ->(&block) { pause_started << true; block.call }, prepare: ->(_fields) { raise 'pause cannot prepare' })
    pauser = Thread.new { pause_driver.pause(payload['requestId']) }
    pause_started.pop
    # An admission already in progress must settle before pause is accepted.
    assert_nil pauser.join(0.05)
    proceed << true
    assert_equal 1, claimer.value.length
    assert_equal 'paused', pauser.value.dig('continuation', 'state')
  ensure
    proceed << true if proceed
    claimer&.join
    pauser&.join
  end

  def test_all_repository_preflight_catches_unpublished_default_before_creating_a_run
    first = owner('preflight')
    payload = integration_request(first)
    git('-C', @bare, 'update-ref', 'refs/heads/master', payload['repositories'][0]['head'])
    integration = driver(first)
    error = assert_raises(WorkspaceCoordination::Error) { integration.start(payload, background: true) }
    assert_match(/code: local refs\/heads\/master has unpublished/, error.message)
    refute File.exist?(integration.path(payload['requestId']))
    assert_empty @engine.store.read['jobs']
    assert_empty @engine.store.read['approvals']
  end

  def test_unprepared_retry_recertifies_tracking_refresh_instead_of_trusting_cached_head
    first = owner('cached-preparation')
    integration = driver(first)
    payload = integration_request(first)
    checkout = File.join(@workspace, 'worktrees', first['slug'], 'code')
    FileUtils.mkdir_p(File.dirname(checkout))
    git('-C', @bare, 'worktree', 'move', File.join(@tmp, first['slug']), checkout)
    original_prepare = integration.instance_variable_get(:@prepare)
    integration.instance_variable_set(:@prepare, ->(*) { raise WorkspaceCoordination::Error.new('fixture preparation stopped', 3) })
    integration.start(payload, background: true)
    integration.run_background(payload['requestId'])
    old = integration.read(payload['requestId']).dig('operations', 'code', 'expectedHead')
    refute integration.read(payload['requestId']).dig('operations', 'code', 'snapshot')
    seed = File.join(@tmp, 'seed')
    FileUtils.mkdir_p(File.join(seed, 'work', 'another'))
    File.write(File.join(seed, 'work', 'another', 'state.md'), 'tracking-only advancement')
    git('-C', seed, 'add', 'work/another/state.md')
    git('-C', seed, 'commit', '-m', 'fixture tracking-only advancement')
    git('-C', seed, 'push', 'origin', 'master')
    integration.instance_variable_set(:@prepare, original_prepare)
    integration.arm_background(payload['requestId'])
    result = integration.run_background(payload['requestId'])
    assert_equal 'merging', result['phase']
    refute_equal old, integration.read(payload['requestId']).dig('operations', 'code', 'expectedHead')
    assert_equal 'reconstructed_patch', integration.read(payload['requestId']).dig('operations', 'code', 'proof', 'relation')
    assert_equal 1, @engine.store.read['jobs'].length
  end

  def test_cancel_unsubmitted_preserves_evidence_and_refuses_any_submission_boundary
    first = owner('cancel-unsubmitted')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    original = integration.read(payload['requestId'])
    result = integration.cancel_unsubmitted(payload['requestId'])
    assert_equal 'canceled', result['outcome']
    assert_equal 'complete', result.dig('continuation', 'state')
    assert_equal original['request'], integration.read(payload['requestId'])['request']
    assert_empty @engine.store.read['jobs']
    assert_equal result, integration.cancel_unsubmitted(payload['requestId'])

    value = original
    value['steps']['submit-batch'] = {'requestId' => SecureRandom.uuid}
    integration.save(value)
    assert_raises(WorkspaceCoordination::Error) { integration.cancel_unsubmitted(payload['requestId']) }
    value['steps'].delete('submit-batch')
    value['jobs']['system'] = SecureRandom.uuid
    integration.save(value)
    assert_raises(WorkspaceCoordination::Error) { integration.cancel_unsubmitted(payload['requestId']) }
    value['jobs'].clear
    value['deploymentComplete'] = true
    integration.save(value)
    assert_raises(WorkspaceCoordination::Error) { integration.cancel_unsubmitted(payload['requestId']) }
  end

  def coordination_host(actor)
    host = Object.new.extend(DevWorkspaceHost::WorkspaceCoordinationHost)
    host.instance_variable_set(:@env, actor ? {'CODEX_THREAD_ID' => actor} : {})
    host.instance_variable_set(:@out, StringIO.new)
    host.instance_variable_set(:@err, StringIO.new)
    engine = @engine
    host.define_singleton_method(:package_root) { engine.package }
    host.define_singleton_method(:coordination_engine) { |_entry| engine }
    host.define_singleton_method(:with_coordination_session_admission) { |*_args, **_options, &block| block.call }
    host
  end

  def test_background_cli_refuses_another_or_unknown_conversation_before_creating_a_run
    first = owner('recipient')
    payload = integration_request(first)
    path = File.join(@tmp, 'integration-request.json')
    File.write(path, JSON.generate(payload))
    [nil, SecureRandom.uuid].each do |actor|
      error = assert_raises(WorkspaceCoordination::Error) do
        coordination_host(actor).coordination_session_command(@entry,
          ['integrate', first['slug'], '--as-is', '--request', path, '--background'])
      end
      assert_equal 4, error.code
      assert_match(/recipient/, error.message)
      refute File.exist?(driver(first).path(payload['requestId']))
    end
    coordination_host(first['thread']).coordination_session_command(@entry,
      ['integrate', first['slug'], '--as-is', '--request', path, '--background'])
    assert_equal 'runnable', driver(first).read(payload['requestId']).dig('continuation', 'state')
    assert_empty @engine.store.read['jobs']
  end

  def test_background_cli_explicitly_hands_to_the_managed_lead_and_rejects_conflicting_identity
    first = owner('explicit-recipient')
    host = coordination_host(SecureRandom.uuid)
    assert_nil host.verify_background_recipient!(first, explicit: true)
    host.instance_variable_set(:@env, {'CODEX_THREAD_ID' => first['thread'], 'CODEX_SESSION_ID' => SecureRandom.uuid})
    assert_raises(WorkspaceCoordination::Error) { host.verify_background_recipient!(first, explicit: true) }
  end

  def test_attention_requires_pickup_after_delivery_and_scheduler_reminds_without_replaying_work
    first = owner('pickup')
    integration = driver(first)
    payload = integration_request(first)
    integration.instance_variable_set(:@prepare, ->(*) { raise WorkspaceCoordination::Error.new('fixture preparation stopped', 3) })
    start = Time.now
    with_time(start) do
      integration.start(payload, background: true)
      result = integration.run_background(payload['requestId'])
      assert_equal 'needs_attention', result.dig('continuation', 'state')
      assert_equal 'pending', result.dig('handoff', 'state')
    end
    sends = []
    @engine.define_singleton_method(:notification_call) do |owner, event, action|
      next {'options' => {'model' => 'retained-model'}} if action == 'prepare'
      sends << notification_payload(owner, event, action)
      {'receipt' => {'turnId' => "turn-#{sends.length}"}}
    end
    with_time(start) { @engine.deliver_notifications(owner_id: first['id']) }
    assert_equal 1, sends.length
    event_id = integration.read(payload['requestId']).dig('handoff', 'eventId')
    assert_equal 'delivered', @engine.store.read['outbox'][event_id]['state']
    assert_equal 'pending', driver(first).status(payload['requestId']).dig('handoff', 'state')
    @engine.define_singleton_method(:dispatch_notifications) { |owner| deliver_notifications(owner_id: owner['id']) }
    with_time(start + 61) { coordination_host(first['thread']).coordinate_owner(@entry, @engine, first) }
    assert_equal 2, sends.length
    refute_equal sends[0]['messageId'], sends[1]['messageId']
    assert_equal sends[0]['options'], sends[1]['options']
    assert_equal sends[0]['text'], sends[1]['text']
    assert_equal 1, @engine.store.read['outbox'].length
    assert_empty @engine.store.read['jobs']

    handoff = integration.read(payload['requestId'])['handoff']
    ack = request('eventId' => handoff['eventId'], 'generation' => handoff['generation'])
    assert_raises(WorkspaceCoordination::Error) { integration.acknowledge(payload['requestId'], ack, recipient: SecureRandom.uuid) }
    assert_raises(WorkspaceCoordination::Error) { integration.acknowledge(payload['requestId'], ack.merge('generation' => 1), recipient: first['thread']) }
    with_time(start + 62) do
      ack_path = File.join(@tmp, 'ack.json')
      File.write(ack_path, JSON.generate(ack))
      host = coordination_host(first['thread'])
      host.coordination_session_command(@entry.merge('root' => @workspace),
        ['integration-ack', first['slug'], payload['requestId'], '--as-is', '--request', ack_path])
      receipt = JSON.parse(host.instance_variable_get(:@out).string)
      assert_equal 'acknowledged', receipt['state']
      assert_equal receipt, integration.acknowledge(payload['requestId'], ack, recipient: first['thread'])
    end
    with_time(start + 200) { coordination_host(first['thread']).coordinate_owner(@entry, @engine, first) }
    assert_equal 2, sends.length
    # A restarted agent that never follows up cannot suppress attention forever.
    with_time(start + 400) { coordination_host(first['thread']).coordinate_owner(@entry, @engine, first) }
    assert_equal 3, sends.length
    assert_equal 'pending', integration.read(payload['requestId']).dig('handoff', 'state')
    assert_empty @engine.store.read['jobs']
  end

  def test_supervised_continuation_keeps_jobs_eligible_and_reports_failure_to_the_attached_caller
    first = owner('supervised')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    original = integration.method(:resume_locked)
    integration.define_singleton_method(:resume_locked) do |id|
      result = original.call(id)
      if result['phase'] == 'merging'
        job = engine.claim(owner_id: first['id']).first
        raise 'supervised jobs were not eligible' unless job
        file = engine.attempt_path(job['attempt'], 'request.json')
        WorkspaceCoordination.atomic(file, engine.worker_request(job, first))
        WorkspaceCoordination::Worker.new(file, fixtures: true).run
        engine.reconcile_attempts
      end
      result
    end
    result = integration.supervise(payload['requestId'])
    assert_equal 'complete', result['phase']
    assert_equal 'complete', result.dig('continuation', 'state')
    assert_equal 1, @engine.store.read['jobs'].length
    assert_empty @engine.store.read['outbox']
    # Resume interruption between phase completion and continuation completion.
    value = integration.read(payload['requestId'])
    value['continuation']['state'] = 'runnable'
    integration.save(value)
    jobs = @engine.store.read['jobs']
    result = driver(first).supervise(payload['requestId'])
    assert_equal 'complete', result.dig('continuation', 'state')
    assert_equal jobs, @engine.store.read['jobs']
  end

  def test_supervision_failure_stays_attached_and_only_explicit_background_resume_releases_it
    first = owner('supervision-failure')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    integration.instance_variable_set(:@prepare, ->(*) { raise WorkspaceCoordination::Error.new('fixture preparation failure', 3) })
    error = assert_raises(WorkspaceCoordination::Error) { integration.supervise(payload['requestId']) }
    assert_equal 3, error.code
    assert_equal 'paused', integration.read(payload['requestId']).dig('continuation', 'state')
    assert_equal 'supervised', integration.read(payload['requestId']).dig('handoff', 'state')
    launches = []
    @engine.define_singleton_method(:capture) do |*argv, **_options|
      next "inactive\n" if argv.first == 'systemctl'
      launches << argv if argv.first == 'systemd-run'
      ''
    end
    @engine.dispatch_integrations(first)
    assert_empty launches
    integration.arm_background(payload['requestId'])
    @engine.dispatch_integrations(first)
    assert_equal 1, launches.length
    assert_empty @engine.store.read['jobs']
  end

  def test_previously_dispatched_worker_rechecks_supervision_under_the_run_lock
    first = owner('delayed-background-worker')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    value = integration.read(payload['requestId'])
    integration.retain_handoff(value, 'needs_attention')
    value['handoff']['state'] = 'supervised'
    # Abrupt supervisor loss leaves runnable state. An already dispatched
    # worker must retain it without preparing or submitting any operation.
    integration.save(value)
    worker = driver(first)
    worker.instance_variable_set(:@prepare, ->(*) { raise 'unattended preparation' })
    jobs = @engine.store.read['jobs']
    result = worker.run_background(payload['requestId'])
    assert_equal 'runnable', result.dig('continuation', 'state')
    assert_equal 'supervised', result.dig('handoff', 'state')
    assert_equal jobs, @engine.store.read['jobs']
    assert_equal value, integration.read(payload['requestId'])
  end

end
