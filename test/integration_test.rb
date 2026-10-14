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
    @engine.define_singleton_method(:notification_call) do |recipient, _event, action|
      action == 'prepare' ? {'options' => {'model' => 'retained'}} : {'receipt' => {'threadId' => recipient['thread']}}
    end
    @engine.deliver_notifications(owner_id: first['id'])
    finished = integration.resume(payload['requestId'])
    assert_equal 'complete', finished['phase']
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
end
