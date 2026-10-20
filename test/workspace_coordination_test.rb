# frozen_string_literal: true

require 'minitest/autorun'
require 'shellwords'
require 'tmpdir'
require 'timeout'
require_relative 'support/coordination_capacity_fixture'
require_relative 'support/coordination_fixture'
require_relative '../libexec/workspace-coordination-execution'
load File.expand_path('../libexec/workspace-host', __dir__)

class WorkspaceCoordinationTest < Minitest::Test
  include CoordinationCapacityFixture
  include CoordinationFixture

  def test_collection_shares_generation_with_sessions_but_excludes_preparation_and_switches
    host = DevWorkspaceHost::Host.new(env: {'HOME' => @tmp, 'DEV_WORKSPACES_STATE' => @state,
      'DEV_WORKSPACES_PROFILE' => @profile, 'DEV_WORKSPACES_CONFIG' => File.join(@tmp, 'registry.json')},
      out: StringIO.new, err: StringIO.new)
    package, engine, entry = @package, @engine, @entry
    host.define_singleton_method(:package_root) { package }
    host.define_singleton_method(:registry) { Struct.new(:entries).new([entry]) }
    host.define_singleton_method(:coordination_engine) { |_entry| engine }
    preparing, release_preparation, collecting, release_collection = 4.times.map { Queue.new }
    host.define_singleton_method(:coordination_prepare_owned) do |*_args|
      preparing << true
      release_preparation.pop
    end
    collector = Object.new
    collector.define_singleton_method(:call) do |apply:|
      collecting << true
      release_collection.pop
      {'mode' => apply ? 'apply' : 'dry-run', 'actions' => [], 'removed' => [], 'preserved' => []}
    end
    original_collector = WorkspaceCoordination::Collector.method(:new)
    WorkspaceCoordination::Collector.define_singleton_method(:new) { |_engine| collector }
    begin
      preparation = Thread.new { host.send(:coordination_prepare, engine, {}, {'kind' => 'merge', 'repository' => 'code'}) }
      Timeout.timeout(5) { preparing.pop }
      collection = Thread.new { host.send(:coordination_collect_command, ['--apply', '--json']) }
      # Collection cannot begin until an already-admitted preparation finishes.
      assert_raises(Timeout::Error) { Timeout.timeout(0.1) { collecting.pop } }
      release_preparation << true
      Timeout.timeout(5) { preparation.value; collecting.pop }
      path = File.join(@state, 'transition.lock')
      File.open(path, File::RDWR) do |session|
        assert session.flock(File::LOCK_SH | File::LOCK_NB), 'session creation must remain available during collection'
      end
      File.open(path, File::RDWR) do |activation|
        refute activation.flock(File::LOCK_EX | File::LOCK_NB), 'package activation must still wait for collection'
      end
      refute engine.store.lock('preparation', mode: File::LOCK_SH, nonblock: true) { true },
        'new payload preparation must still wait for collection'
      release_collection << true
      Timeout.timeout(5) { collection.value }
    ensure
      [preparation, collection].compact.each { |thread| thread.kill.join if thread.alive? }
      WorkspaceCoordination::Collector.define_singleton_method(:new, original_collector)
    end
  end

  def test_admitted_collection_excludes_candidate_publication_and_retained_job_grants
    configure_machine
    @catalog['repositories']['workspace'] = @catalog['repositories'].fetch('code').dup
    install_catalog
    @engine = engine
    first, second = owner('collected-candidate'), owner('retained-grant')
    checkout = feature(first)
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    snapshot = @engine.prepare_merge(first, 'code', evidence(first))
    finished = @engine.mutate(first, 'submit', request('kind' => 'merge', 'dependsOn' => [],
      'approvalId' => approval(first, snapshot), 'reservationId' => nil, 'snapshot' => snapshot))
    @engine.mutate(first, 'cancel', request('expectedRevision' => finished['revision'], 'reason' => 'finished fixture'), job_id: finished['id'])
    queued = submit_merge(second)
    host = DevWorkspaceHost::Host.new(env: {'HOME' => @tmp, 'DEV_WORKSPACES_STATE' => @state,
      'DEV_WORKSPACES_PROFILE' => @profile}, out: StringIO.new, err: StringIO.new)
    prepared = {'schema' => 1, 'kind' => 'profile', 'candidatePackage' => @package,
      'profileToken' => DevWorkspaceProfileIdentity.token(@profile),
      'sourceIdentity' => host.send(:coordination_source_identity, checkout)}
    removing, finish_removal, publishing = 3.times.map { Queue.new }
    paused = false
    original_capture = @engine.method(:capture)
    @engine.define_singleton_method(:capture) do |*args, **options|
      if !paused && args.include?('worktree') && args.include?('remove')
        paused = true
        removing << true
        finish_removal.pop
      end
      original_capture.call(*args, **options)
    end
    original_retain = @engine.method(:retain_commit)
    @engine.define_singleton_method(:retain_commit) do |*args, **options|
      publishing << true
      original_retain.call(*args, **options)
    end
    collector = WorkspaceCoordination::Collector.new(@engine)
    collection = Thread.new { @engine.store.lock('preparation') { collector.call(apply: true) } }
    Timeout.timeout(5) { removing.pop }
    # Removal has passed its final resource check and is paused at Git itself.
    assert_empty Timeout.timeout(1) { @engine.claim(owner_id: second['id']) }
    assert_nil @engine.store.read['jobs'].fetch(queued['id'])['attempt']
    candidate = Thread.new do
      host.send(:prepare_candidate_snapshot, @engine, first, checkout, head, prepared, {'evidence' => evidence(first)})
    end
    assert_raises(Timeout::Error) { Timeout.timeout(0.1) { publishing.pop } }
    finish_removal << true
    result = Timeout.timeout(5) { collection.value }
    assert_equal 2, result['removed'].count { |item| item['kind'] == 'worktree' }
    fresh = Timeout.timeout(5) { candidate.value }
    artifact = @engine.artifact!(fresh.dig('operation', 'preparedArtifact'))
    assert File.directory?(artifact['source'])
    assert_equal head, git('-C', artifact['source'], 'rev-parse', 'HEAD').strip
    assert_equal queued['id'], @engine.claim(owner_id: second['id']).first['id']
  ensure
    [collection, candidate].compact.each { |thread| thread.kill.join if thread.alive? }
  end

  def test_sensitive_owner_prepares_local_evidence_without_tracking_commit
    first = owner('sensitive')
    manifest_path = File.join(@workspace, 'work', 'sensitive', 'portal.yml')
    manifest = YAML.safe_load(File.read(manifest_path))
    manifest['sensitive'] = true
    File.write(manifest_path, YAML.dump(manifest))
    git('init', '--initial-branch=master', @workspace)
    File.open(File.join(@workspace, '.git', 'info', 'exclude'), 'a') { |f| f.puts('/work/sensitive/') }
    assert_equal first['id'], @engine.owner('sensitive')['id']
    feature(first)
    snapshot = @engine.prepare_merge(first, 'code', evidence(first))
    assert_equal evidence(first), snapshot['evidence']
    saved_path = File.join(@engine.store.root, 'sources', snapshot['id'].delete_prefix('sha256:'), 'snapshot.json')
    assert_equal first['id'], JSON.parse(File.read(saved_path))['ownerId']
    assert_equal '', git('-C', @workspace, 'ls-files', '--', 'work/sensitive')
  end

  def test_prepared_hook_path_preserves_transport_and_restores_environment
    tools = File.join(@tmp, 'hook-tools')
    FileUtils.mkdir_p(tools)
    File.write(File.join(tools, 'prepared-hook'), "#!/bin/sh\nprintf 'hook ready'\n")
    File.chmod(0o700, File.join(tools, 'prepared-hook'))
    artifact = @engine.save_artifact('kind' => 'merge_hooks', 'repository' => 'code',
      'featureHead' => @base, 'environment' => {'PATH' => tools, 'BUNDLE_PATH' => tools})
    original = @engine.instance_variable_get(:@env)
    @engine.with_merge_hooks('hookArtifact' => artifact['id']) do
      assert_equal 'hook ready', @engine.capture('prepared-hook')
      assert_match(/git version/, @engine.capture('git', '--version'))
      @engine.capture('ssh', '-V')
    end
    assert_same original, @engine.instance_variable_get(:@env)
    assert_raises(RuntimeError) do
      @engine.with_merge_hooks('hookArtifact' => artifact['id']) { raise 'fixture failure' }
    end
    assert_same original, @engine.instance_variable_get(:@env)
  end

  def test_hook_preparation_enters_initialized_shell_and_captures_only_allowlisted_values
    source = File.join(@tmp, 'hook-source')
    tools = File.join(@tmp, 'preparation-tools')
    prepared_tools = File.join(source, 'prepared-tools')
    FileUtils.mkdir_p([source, tools, prepared_tools, File.join(source, '.husky')])
    ruby = File.join(prepared_tools, 'ruby')
    File.write(ruby, "#!/bin/sh\nprintf 'selected\\n' > \"$0.selected\"\nexec #{Shellwords.escape(RbConfig.ruby)} \"$@\"\n")
    File.chmod(0o700, ruby)
    File.write(File.join(source, 'flake.nix'), 'fixture declared shell')
    script = <<~SH
      #!/bin/sh
      test "$1" = develop || exit 41
      test "$2" = "$PWD" || exit 42
      test "$3" = --command || exit 43
      shift 3
      test -z "${BUNDLE_PATH+x}" || exit 44
      export PATH="$PWD/prepared-tools"
      export BUNDLE_PATH="$PWD/prepared-gems"
      export BUNDLE_APP_CONFIG="$PWD/prepared-config"
      export PRIVATE_FIXTURE_SECRET=do-not-capture
      printf 'fixture shell initialized\\n'
      exec "$@"
    SH
    File.write(File.join(tools, 'nix'), script)
    File.chmod(0o700, File.join(tools, 'nix'))
    @engine.instance_variable_set(:@env, ENV.to_h.merge('PATH' => tools, 'BUNDLE_PATH' => '/wrong/inherited/path'))
    original = @engine.instance_variable_get(:@env)
    artifact = @engine.prepare_merge_hooks(source, 'code', @base)
    assert File.file?("#{ruby}.selected"), 'preparation must select Ruby from the initialized repository shell PATH'
    assert_equal prepared_tools, artifact['environment']['PATH']
    assert_equal File.join(source, 'prepared-gems'), artifact['environment']['BUNDLE_PATH']
    assert_equal File.join(source, 'prepared-config'), artifact['environment']['BUNDLE_APP_CONFIG']
    refute_includes artifact['environment'].keys, 'PRIVATE_FIXTURE_SECRET'
    assert_same original, @engine.instance_variable_get(:@env)
    assert_equal '/wrong/inherited/path', @engine.instance_variable_get(:@env)['BUNDLE_PATH']
  end

  def test_missing_overcommit_dependency_refuses_during_preparation
    source = File.join(@tmp, 'missing-hook-source')
    tools = File.join(@tmp, 'missing-preparation-tools')
    FileUtils.mkdir_p([source, tools])
    File.write(File.join(source, '.overcommit.yml'), "gemfile: Gemfile\n")
    File.write(File.join(source, 'Gemfile'), "source 'https://rubygems.org'\ngem 'absent_coordination_fixture', '= 0.0.0'\n")
    File.write(File.join(source, 'flake.nix'), 'fixture declared shell')
    File.write(File.join(tools, 'nix'), "#!/bin/sh\nshift 3\nexec \"$@\"\n")
    File.chmod(0o700, File.join(tools, 'nix'))
    File.symlink(RbConfig.ruby, File.join(tools, 'ruby'))
    @engine.instance_variable_set(:@env, ENV.to_h.merge('PATH' => tools))
    original = @engine.instance_variable_get(:@env)
    error = assert_raises(WorkspaceCoordination::Error) { @engine.prepare_merge_hooks(source, 'code', @base) }
    assert_includes error.message, 'absent_coordination_fixture'
    assert_same original, @engine.instance_variable_get(:@env)
    assert_empty @engine.store.read['jobs']
  end

  def test_fresh_merge_source_initializes_dependencies_before_real_checkout_hooks
    first = owner('fresh-hook-source')
    checkout = feature(first)
    FileUtils.mkdir_p(File.join(checkout, '.husky'))
    File.write(File.join(checkout, '.husky/pre-commit'), 'fixture hook declaration')
    File.write(File.join(checkout, 'flake.nix'), 'fixture declared shell')
    git('-C', checkout, 'add', '.husky', 'flake.nix')
    git('-C', checkout, 'commit', '-m', 'fixture hook declaration')
    calls = File.join(@tmp, 'checkout-hook-calls')
    hook = File.join(@bare, 'hooks/post-checkout')
    File.write(hook, "#!/bin/sh\nprepared-checkout-dependency || exit 43\nprintf '%s\\n' \"$PWD\" >> '#{calls}'\n")
    File.chmod(0o700, hook)
    tools = File.join(@tmp, 'fresh-preparation-tools')
    FileUtils.mkdir_p(tools)
    fail_once = File.join(@tmp, 'fail-first-shell-init')
    File.write(fail_once, 'fixture initialization failure')
    script = <<~SH
      #!/bin/sh
      test "$1" = develop || exit 41
      test "$2" = "$PWD" || exit 42
      shift 3
      if [ -f "#{fail_once}" ]; then
        rm "#{fail_once}"
        exit 44
      fi
      mkdir -p "$PWD/.prepared-tools"
      printf '#!/bin/sh\\nexit 0\\n' > "$PWD/.prepared-tools/prepared-checkout-dependency"
      chmod 700 "$PWD/.prepared-tools/prepared-checkout-dependency"
      export PATH="$PWD/.prepared-tools:$PATH"
      exec "$@"
    SH
    File.write(File.join(tools, 'nix'), script)
    File.chmod(0o700, File.join(tools, 'nix'))
    # Ignore the fixture's shell-created tools, as real repository shells do.
    File.open(File.join(@bare, 'info/exclude'), 'a') { |file| file.puts '.prepared-tools/' }
    @engine.instance_variable_set(:@env, ENV.to_h.merge('PATH' => [tools, ENV.fetch('PATH')].join(File::PATH_SEPARATOR)))
    assert_raises(WorkspaceCoordination::Error) { @engine.prepare_merge(first, 'code', evidence(first)) }
    refute File.exist?(calls)
    assert_empty @engine.store.read['jobs']
    snapshot = @engine.prepare_merge(first, 'code', evidence(first))
    recorded = File.readlines(calls, chomp: true)
    assert_equal 2, recorded.length
    assert_includes recorded, snapshot['operation']['targetCheckout']
    assert_equal snapshot['operation']['featureHead'], git('-C', recorded.first, 'rev-parse', 'HEAD').strip
    assert_empty @engine.store.read['jobs']
  end

  def test_event_dates_track_committed_states_and_idempotent_retries
    first = owner('dated-events')
    job = with_time(Time.utc(2026, 1, 1)) { submit_merge(first) }
    timing = WorkspaceCoordination::Timing.new(@engine.store.root)
    assert_equal '2026-01-01T00:00:00Z', timing.read['jobs'][job['id']]['createdAt']
    payload = request('expectedRevision' => job['revision'], 'reason' => 'finished fixture')
    with_time(Time.utc(2026, 1, 2)) { @engine.mutate(first, 'cancel', payload, job_id: job['id']) }
    dates = timing.read['jobs'][job['id']]
    assert_equal '2026-01-01T00:00:00Z', dates['createdAt']
    assert_equal '2026-01-02T00:00:00Z', dates['stateChangedAt']
    assert_equal dates['stateChangedAt'], dates['finishedAt']
    before = File.binread(File.join(@engine.store.root, 'timing.json'))
    with_time(Time.utc(2026, 1, 3)) { @engine.mutate(first, 'cancel', payload, job_id: job['id']) }
    assert_equal before, File.binread(File.join(@engine.store.root, 'timing.json'))
    assert_equal dates.slice(*WorkspaceCoordination::Timing::FIELDS), @engine.status(first['slug'], portal: true)['jobs'].first['timestamps']
  end

  def test_missing_corrupt_and_stale_dates_never_repair_on_status_reads
    first = owner('legacy-dates')
    job = submit_merge(first)
    path = File.join(@engine.store.root, 'timing.json')
    File.unlink(path)
    assert_empty @engine.status(first['slug'], portal: true)['jobs'].first['timestamps']
    refute File.exist?(path)
    File.write(path, '{corrupt')
    assert_empty @engine.status(first['slug'], portal: true)['jobs'].first['timestamps']
    assert_equal '{corrupt', File.read(path)
    timing = WorkspaceCoordination::Timing.new(@engine.store.root)
    ledger = @engine.store.read
    metadata = timing.empty
    metadata['jobs'][job['id']] = {'revision' => job['revision'] + 1, 'state' => 'succeeded', 'finishedAt' => '2026-01-01T00:00:00Z'}
    WorkspaceCoordination.atomic(path, metadata)
    assert_empty @engine.status(first['slug'], portal: true)['jobs'].first['timestamps']
    assert_equal ledger, @engine.store.read
    payload = request('expectedRevision' => job['revision'], 'reason' => 'new event')
    @engine.mutate(first, 'cancel', payload, job_id: job['id'])
    dates = @engine.status(first['slug'], portal: true)['jobs'].first['timestamps']
    refute dates.key?('createdAt')
    assert dates.key?('finishedAt')
  end

  def test_timing_write_failure_does_not_fail_committed_work_or_scheduler_wake
    first = owner('unavailable-dates')
    job = submit_merge(first)
    path = File.join(@engine.store.root, 'timing.json')
    File.unlink(path)
    FileUtils.mkdir_p(path)
    wakes = 0
    @engine.store.define_singleton_method(:wake) { wakes += 1 }
    payload = request('expectedRevision' => job['revision'], 'reason' => 'still successful')
    _out, err = capture_io { assert_equal 'canceled', @engine.mutate(first, 'cancel', payload, job_id: job['id'])['state'] }
    assert_match(/coordination timing is unavailable/, err)
    assert_equal 1, wakes
    assert_empty @engine.status(first['slug'], portal: true)['jobs'].first['timestamps']
    @engine.store.validate!(@engine.store.read)
  end

  def test_reservation_grant_time_is_distinct_from_request_and_covers_lifecycle_release
    first = owner('dated-reservation')
    @engine.store.transaction do |data|
      id = SecureRandom.uuid
      data['reservations'][id] = {'id' => id, 'ownerId' => first['id'], 'machine' => 'fixture:buildhost', 'target' => 'buildhost',
        'state' => 'requested', 'revision' => 1, 'sequence' => @engine.store.sequence(data), 'mode' => 'deploy_first',
        'approvalId' => nil, 'integrationObligations' => [], 'history' => []}
      @reservation_id = id
    end
    timing = WorkspaceCoordination::Timing.new(@engine.store.root)
    refute timing.read['reservations'][@reservation_id].key?('heldAt')
    with_time(Time.utc(2026, 1, 2)) do
      @engine.store.transaction do |data|
        value = data['reservations'][@reservation_id]; value['state'] = 'held'; value['revision'] += 1
      end
    end
    assert_equal '2026-01-02T00:00:00Z', timing.read['reservations'][@reservation_id]['heldAt']
    archive_id = '7' * 64
    with_time(Time.utc(2026, 1, 3)) { @engine.store.bind_lifecycle(@workspace, first['slug'], 'archive', archive_id) }
    refute timing.read['reservations'][@reservation_id].key?('finishedAt')
    with_time(Time.utc(2026, 1, 4)) { @engine.store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: archive_id) }
    dates = timing.read['reservations'][@reservation_id]
    assert_equal '2026-01-02T00:00:00Z', dates['heldAt']
    assert_equal '2026-01-04T00:00:00Z', dates['finishedAt']
  end

  def test_session_projection_excludes_a_recreated_slugs_retained_history
    first = owner('replacement-history')
    submit_merge(first)
    assert_equal 1, @engine.status(first['slug'], portal: true)['jobs'].length
    directory = File.join(@workspace, 'work', first['slug'])
    File.rename(directory, File.join(@tmp, 'previous-session'))
    owner(first['slug'])
    result = @engine.status(first['slug'], portal: true)
    assert_empty result['jobs']
    assert_equal 1, result['owners'].length
    assert result['owners'].first['observationIdentity']
  end

  def fill_admission(limit)
    @engine.store.transaction do |data|
      if limit == :receipts
        16_384.times do
          data['receipts'][SecureRandom.uuid] = {'digest' => 'sha256:' + ('0' * 64), 'result' => {}}
        end
      else
        size = WorkspaceCoordination::MAX_LEDGER - WorkspaceCoordination::RECOVERY_RESERVE
        chunk = {'retained' => 'x' * (WorkspaceCoordination::MAX_RESULT - 128)}
        count = size / (JSON.generate(chunk).bytesize + 160)
        count.times do
          data['receipts'][SecureRandom.uuid] = {'digest' => 'sha256:' + ('0' * 64), 'result' => chunk}
        end
      end
    end
  end

  def test_cancel_can_use_recovery_capacity_at_both_admission_limits
    [:receipts, :bytes].each do |limit|
      first = owner("cancel-#{limit}")
      job = submit_merge(first)
      fill_admission(limit)
      current = @engine.store.read['jobs'].fetch(job['id'])
      payload = request('expectedRevision' => current['revision'], 'reason' => 'deployment canceled')
      assert_equal 'canceled', @engine.mutate(first, 'cancel', payload, job_id: job['id'])['state']
      assert_equal 'canceled', @engine.mutate(first, 'cancel', payload, job_id: job['id'])['state']
      @engine.store.transaction { |data| data['receipts'].clear }
    end
  end

  def test_unavailable_foreign_owner_cannot_starve_notification_delivery
    first = owner('unavailable')
    submit_merge(first)
    @engine.claim
    @engine.store.transaction { |data| 2.times { @engine.event(data, first, {'id' => SecureRandom.uuid}, 'ready') } }
    @workspace = File.join(@tmp, 'other-workspace')
    FileUtils.mkdir_p(@workspace)
    @engine = engine
    second = owner('healthy')
    @engine.store.transaction { |data| @engine.event(data, second, {'id' => SecureRandom.uuid}, 'ready') }
    calls = []
    @engine.define_singleton_method(:notification_call) do |recipient, event, action|
      calls << recipient['id']
      action == 'prepare' ? {'options' => {'model' => 'retained'}} : {'receipt' => {'threadId' => event['thread']}}
    end
    @engine.deliver_notifications(owner_id: second['id'])
    assert_equal [second['id'], second['id']], calls
    assert @engine.store.read['outbox'].values.any? { |event| event['ownerId'] == second['id'] && event['state'] == 'delivered' }
    assert @engine.store.read['outbox'].values.select { |event| event['ownerId'] == first['id'] }.all? { |event| event['state'] == 'pending' }
  end

  def test_retained_notification_admission_preserves_completion_and_lifecycle_guards
    first = owner('completed-notification')
    state_path = File.join(@workspace, 'work', first['slug'], 'state.md')
    File.write(state_path, "---\nlifecycle: complete\n---\n")
    before = @engine.store.read
    assert_equal first, @engine.notification_owner(first)
    assert_equal before, @engine.store.read
    assert_raises(WorkspaceCoordination::Error) { @engine.owner(first['slug']) }
    assert_raises(WorkspaceCoordination::Error) { @engine.notification_owner(first.merge('id' => SecureRandom.uuid)) }
    @engine.store.transaction { |data| data['suspended'] = true }
    assert_raises(WorkspaceCoordination::Error) { @engine.notification_owner(first) }
    @engine.store.transaction do |data|
      data['suspended'] = false
      data['owners'][first['id']]['fence'] = {'id' => SecureRandom.uuid, 'command' => 'archive', 'completed' => false, 'operationId' => nil}
    end
    assert_raises(WorkspaceCoordination::Error) { @engine.notification_owner(first) }
    @engine.store.transaction { |data| data['owners'][first['id']]['fence'] = nil }
    File.write(state_path, "---\nlifecycle: abandoned\n---\n")
    assert_raises(WorkspaceCoordination::Error) { @engine.notification_owner(first) }
    File.rename(File.dirname(state_path), File.join(@tmp, 'previous-tracking'))
    owner(first['slug'])
    assert_raises(WorkspaceCoordination::Error) { @engine.notification_owner(first) }
  end

  def test_machine_revalidates_remote_configuration_before_any_activation
    configure_machine
    first = owner('deployer')
    checkout = feature(first)
    base, package = @base, @package
    @engine.define_singleton_method(:adapter_call) do |_target, action, _payload|
      raise 'activation must never run' unless action == 'prepare'
      {'observed' => {'runningSystem' => 'baseline'}, 'artifact' => {'configurationRevision' => capture('git', '-C', checkout, 'rev-parse', 'HEAD').strip,
        'inputsInfo' => {}, 'toplevel' => package, 'environmentPackage' => package}}
    end
    snap = @engine.prepare_machine(first, 'alias', evidence(first))
    auth = approval(first, snap, kind: 'deploy', scope: [{'machine' => 'fixture:machine', 'actions' => ['switch'], 'reboot' => false}])
    reservation = @engine.mutate(first, 'reserve', request('target' => 'alias', 'mode' => 'deploy_first', 'approvalId' => auth, 'integrationObligations' => []))
    second = owner('merged-configuration')
    upstream = feature(second)
    git('-C', upstream, 'push', 'origin', 'HEAD:master')
    job = @engine.mutate(first, 'submit', request('kind' => 'machine_activate', 'dependsOn' => [], 'approvalId' => auth, 'reservationId' => reservation['id'], 'snapshot' => snap))
    execute(@engine.claim(owner_id: first['id']).first)
    current = @engine.store.read['jobs'].fetch(job['id'])
    assert_equal 'needs_refresh', current['state']
    assert_match(/configuration target advanced/, current['result']['error'])
    assert_equal 'admitted', current['result']['phase']
    assert_empty @engine.store.read['reservations'][reservation['id']]['history']
  end

  def test_lifecycle_publication_excludes_competing_admission_and_retries_at_capacity
    first, second = owner('publishing'), owner('competing')
    store = @engine.store
    op = 'a' * 64
    journal = File.join(@tmp, 'ordinary-journal')
    store.require_lifecycle_capacity!(@workspace, first['slug'], 'archive')
    store.bind_lifecycle(@workspace, first['slug'], 'archive', op) do
      File.write(journal, op)
      reader, writer = IO.pipe
      child = fork do
        reader.close
        available = WorkspaceCoordination::Store.new(@state).lock(nonblock: true) { true }
        writer.write(available ? 'admitted' : 'excluded')
        writer.close
        exit! 0
      end
      writer.close
      assert_equal 'excluded', reader.read
      reader.close
      Process.wait(child)
      assert $?.success?
    end
    assert_equal op, File.read(journal)
    fill_coordination_admission_bytes(store)
    assert_raises(WorkspaceCoordination::Error) { store.require_lifecycle_capacity!(@workspace, second['slug'], 'delete') }
    store.bind_lifecycle(@workspace, first['slug'], 'archive', op)
    store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: op)
    assert store.read['lifecycles'][op]['completed']
    assert_nil store.read['owners'][second['id']]['fence']
  end

  def test_persisted_artifact_result_and_native_request_boundaries_round_trip
    artifact = {'kind' => 'boundary', 'payload' => ''}
    artifact['payload'] = 'x' * (WorkspaceCoordination::MAX_REQUEST - 1 - JSON.generate(artifact.merge('id' => 'sha256:' + '0' * 64)).bytesize)
    saved = @engine.save_artifact(artifact)
    path = File.join(@engine.store.root, 'sources', saved['id'].delete_prefix('sha256:'), 'artifact.json')
    assert_equal WorkspaceCoordination::MAX_REQUEST, File.size(path)
    assert_equal saved, @engine.artifact!(saved['id'])
    assert_raises(WorkspaceCoordination::Error) { @engine.save_artifact(artifact.merge('payload' => artifact['payload'] + 'x')) }

    first = owner('result-boundary')
    job = submit_merge(first)
    claimed = @engine.claim(owner_id: first['id']).first
    worker = WorkspaceCoordination::Worker.new(write_attempt(claimed), fixtures: true)
    worker.result('applied', '')
    result_path = @engine.attempt_path(claimed['attempt'], 'result.json')
    observation = 'x' * (WorkspaceCoordination::MAX_RESULT - File.size(result_path))
    worker.result('applied', observation)
    assert_equal WorkspaceCoordination::MAX_RESULT, File.size(result_path)
    assert_equal observation, WorkspaceCoordination.private_read(result_path, limit: WorkspaceCoordination::MAX_RESULT)['observed']
    assert_raises(WorkspaceCoordination::Error) { worker.result('applied', observation + 'x') }
    @engine.reconcile_attempts
    assert_equal 'succeeded', @engine.store.read['jobs'][job['id']]['state']

    event = {'text' => 'boundary', 'messageId' => SecureRandom.uuid, 'actionContext' => 'boundary', 'options' => nil}
    options = {'baseInstructions' => ''}
    options_artifact = @engine.save_artifact('kind' => 'notification_options', 'options' => options)
    event['options'] = options_artifact['id']
    payload = @engine.notification_payload(first, event, 'send')
    options['baseInstructions'] = 'x' * (WorkspaceCoordination::MAX_REQUEST - 1 - JSON.generate(payload).bytesize)
    event['options'] = @engine.save_artifact('kind' => 'notification_options', 'options' => options)['id']
    @engine.define_singleton_method(:capture) do |*args, **_|
      file = args[args.index('--request') + 1]
      value = WorkspaceCoordination.private_read(file, limit: WorkspaceCoordination::MAX_REQUEST)
      JSON.generate('size' => File.size(file), 'instructions' => value['options']['baseInstructions'].bytesize)
    end
    response = @engine.notification_call(first, event, 'send')
    assert_equal WorkspaceCoordination::MAX_REQUEST, response['size']
    assert_equal options['baseInstructions'].bytesize, response['instructions']
    options['baseInstructions'] += 'x'
    event['options'] = @engine.save_artifact('kind' => 'notification_options', 'options' => options)['id']
    assert_raises(WorkspaceCoordination::Error) { @engine.notification_call(first, event, 'send') }
  end

  def test_lifecycle_receipts_are_exact_and_idempotent_after_finalization
    first = owner('retained')
    store = @engine.store
    op = 'c' * 64
    store.bind_lifecycle(@workspace, first['slug'], 'archive', op)
    assert_raises(WorkspaceCoordination::Error) { store.lifecycle_completed(@workspace, first['slug'], 'delete', operation_id: op) }
    store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: op)
    store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: op)
    store.revived(@workspace, first['slug'], operation_id: 'd' * 64, archive_operation_id: op)
    store.revived(@workspace, first['slug'], operation_id: 'd' * 64, archive_operation_id: op)
    assert_nil store.read['owners'][first['id']]['fence']
    assert store.read['lifecycles']['d' * 64]['completed']
    FileUtils.remove_entry(File.join(@workspace, 'work', first['slug']))
    replacement = owner(first['slug'])
    assert_equal first['id'], store.read['lifecycles'][op]['owners'].first['id']
    assert_nil store.read['owners'][replacement['id']]['fence']
  end

  def test_revive_without_archive_operation_field_freezes_the_exact_archive
    first = owner('complete-archive')
    manifest_path = File.join(@workspace, 'work', first['slug'], 'portal.yml')
    initial = YAML.safe_load(File.read(manifest_path))
    initial['creation'] = {'state' => 'ready'}
    File.write(manifest_path, YAML.dump(initial))
    first = @engine.owner(first['slug'])
    store = @engine.store
    store.bind_lifecycle(@workspace, first['slug'], 'archive', 'e' * 64)
    store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: 'e' * 64)
    store.bind_revival(@workspace, first['slug'], operation_id: 'f' * 64)
    manifest = File.join(@workspace, 'work', first['slug'], 'portal.yml')
    value = YAML.safe_load(File.read(manifest))
    value['creation'] = {'state' => 'ready', 'tracking_origin' => 'revived'}
    File.write(manifest, YAML.dump(value))
    store.revived(@workspace, first['slug'], operation_id: 'f' * 64)
    assert_nil store.read['owners'][first['id']]['fence']
    assert_equal 'e' * 64, store.read['lifecycles']['f' * 64]['archiveOperationId']
    assert_equal first['id'], @engine.owner(first['slug'])['id']
  end

  def test_resume_preserves_held_reservation_and_queued_job_then_claims
    configure_machine
    first, second = owner('holder'), owner('queued')
    held, active = deployment(first)
    _waiting, queued = deployment(second)
    @engine.claim(owner_id: first['id'])
    @engine.store.transaction do |data|
      data['jobs'][active['id']]['state'] = 'succeeded'
      data['suspended'] = true
    end
    before = @engine.store.read.reject { |key, _| %w[revision suspended].include?(key) }
    assert_empty @engine.claim
    File.write(File.join(@package, 'share/dev-workspace/coordination-contract.json'), JSON.generate(WorkspaceCoordination::CONTRACT))
    @catalog['adapters']['fixture']['command'] = RbConfig.ruby
    @catalog['repositories']['code']['remote'] = 'git@github.com:example/code.git'
    install_catalog
    host = DevWorkspaceHost::Host.new(env: {'HOME' => @tmp, 'DEV_WORKSPACES_STATE' => @state,
      'DEV_WORKSPACES_PROFILE' => @profile, 'DEV_WORKSPACES_CONFIG' => File.join(@tmp, 'registry.json')}, out: StringIO.new, err: StringIO.new)
    package = @package
    host.define_singleton_method(:package_root) { package }
    host.define_singleton_method(:configure_user_services) {}
    host.define_singleton_method(:reconcile_codex_update) { |**| }
    host.define_singleton_method(:configure_auto_archive_services) {}
    assert_equal 0, host.run('workspace-host', ['resume']), host.instance_variable_get(:@err).string
    refute @engine.store.read['suspended']
    assert_equal before, @engine.store.read.reject { |key, _| %w[revision suspended].include?(key) }
    assert_empty @engine.claim(owner_id: second['id'])
    assert_equal 'held', @engine.store.read['reservations'][held['id']]['state']
    assert_equal 'waiting_resource', @engine.store.read['jobs'][queued['id']]['state']
  end

  def test_process_race_has_one_idempotent_receipt
    store = @engine.store
    payload = request('value' => 'same')
    children = 2.times.map do
      fork do
        store.transaction do |data|
          store.request(data, 'fixture', 'race', payload) do
            {'id' => SecureRandom.uuid, 'sequence' => store.sequence(data)}
          end
        end
        exit! 0
      end
    end
    children.each { |pid| Process.wait(pid); assert $?.success? }
    data = store.read
    assert_equal 1, data['receipts'].length
    assert_equal 2, data['nextSequence']
    assert_raises(WorkspaceCoordination::Error) { store.transaction { |d| store.request(d, 'fixture', 'race', payload.merge('value' => 'different')) {} } }
  end

  def test_state_change_wakes_scheduler_and_missing_listener_is_harmless
    store = @engine.store
    FileUtils.mkdir_p(store.root, mode: 0o700)
    path = File.join(store.root, 'wake.sock')
    socket = Socket.new(Socket::AF_UNIX, Socket::SOCK_DGRAM, 0)
    socket.bind(Socket.sockaddr_un(path))
    store.transaction { |data| data['suspended'] = true }
    assert IO.select([socket], nil, nil, 1), 'committed state must wake scheduler'
    assert_equal '.', socket.recv(64)
    socket.close
    File.unlink(path)
    store.transaction { |data| data['suspended'] = false }
    refute store.read['suspended']
  ensure
    socket&.close unless socket&.closed?
  end

  def test_stale_second_merge_does_not_mutate_target
    first, second = owner('first'), owner('second')
    a, b = submit_merge(first), submit_merge(second)
    claims = @engine.claim
    assert_equal [a['id']], claims.map { |job| job['id'] }
    execute(claims.first)
    assert_equal 'succeeded', @engine.store.read['jobs'][a['id']]['state']
    before = git('-C', @bare, 'rev-parse', 'master')
    execute(@engine.claim.first)
    assert_equal before, git('-C', @bare, 'rev-parse', 'master')
    data = @engine.store.read
    assert_equal 'needs_refresh', data['jobs'][b['id']]['state']
    assert data['outbox'].values.any? { |event| event['ownerId'] == second['id'] && event['text'].include?('needs_refresh') }
  end

  def execute(job)
    path = write_attempt(job)
    WorkspaceCoordination::Worker.new(path, fixtures: true).run
    @engine.reconcile_attempts
  end

  def contend_worker(lock_kind)
    first = owner('contended-worker')
    submit_merge(first)
    job = @engine.claim.first
    path = write_attempt(job)
    lock_path = case lock_kind
    when :transition then File.join(@state, 'transition.lock')
    when :session then File.join(@entry['authority'], "#{first['slug']}.lock")
    when :git then File.join(@bare, 'dev-session-tracking.lock')
    end
    FileUtils.mkdir_p(File.dirname(lock_path))
    File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
      lock.flock(File::LOCK_EX)
      worker = WorkspaceCoordination::Worker.new(path, fixtures: true)
      started = Queue.new
      boundary = lock_kind == :git ? :merge : :admit!
      worker.define_singleton_method(boundary) { started << true; super() }
      thread = Thread.new { worker.run }
      begin
        Timeout.timeout(5) { started.pop }
        refute thread.join(0.2), 'temporary lock contention must wait in the original attempt'
        refute File.exist?(File.join(File.dirname(path), 'result.json'))
        assert_equal @base, git('-C', @remote, 'rev-parse', 'master').strip
        assert_equal @base, git('-C', @bare, 'rev-parse', 'master').strip
        yield first, job if block_given?
      ensure
        lock.flock(File::LOCK_UN)
        assert thread.join(5), 'worker must wake after lock release'
      end
    end
    @engine.reconcile_attempts
    [@engine.store.read['jobs'].fetch(job['id']), WorkspaceCoordination.private_read(File.join(File.dirname(path), 'result.json'))]
  end

  def test_worker_waits_for_transition_admission
    job, result = contend_worker(:transition)
    assert_equal 'succeeded', job['state']
    assert_equal 'applied', result['outcome']
    assert_equal job.dig('snapshot', 'operation', 'featureHead'), git('-C', @remote, 'rev-parse', 'master').strip
  end

  def test_worker_waits_for_session_admission
    job, result = contend_worker(:session)
    assert_equal 'succeeded', job['state']
    assert_equal 'applied', result['outcome']
  end

  def test_worker_waits_for_shared_git_mutation
    job, result = contend_worker(:git)
    assert_equal 'succeeded', job['state']
    assert_equal 'applied', result['outcome']
  end

  def test_worker_revalidates_generation_after_transition_wait
    job, result = contend_worker(:transition) do
      replacement = File.join(@tmp, 'replacement-profile')
      File.symlink(@package, replacement)
      File.rename(replacement, @profile)
    end
    assert_equal 'needs_refresh', job['state']
    assert_equal 'baseline_changed', result['outcome']
    assert_nil result['phase']
    assert_match(/generation changed/, result['error'])
    assert_equal @base, git('-C', @remote, 'rev-parse', 'master').strip
  end

  def test_worker_revalidates_admission_after_session_wait
    job, result = contend_worker(:session) { @engine.store.transaction { |data| data['suspended'] = true } }
    assert_equal 'failed', job['state']
    assert_equal 'not_applied', result['outcome']
    assert_nil result['phase']
    assert_match(/no longer admitted/, result['error'])
    assert_equal @base, git('-C', @remote, 'rev-parse', 'master').strip
  end

  def test_worker_revalidates_remote_after_git_wait
    advanced = nil
    job, result = contend_worker(:git) do
      seed = File.join(@tmp, 'seed')
      File.write(File.join(seed, 'base'), 'advanced')
      git('-C', seed, 'commit', '-am', 'concurrent fixture update')
      git('-C', seed, 'push', 'origin', 'master')
      advanced = git('-C', seed, 'rev-parse', 'master').strip
    end
    assert_equal 'needs_refresh', job['state']
    assert_equal 'baseline_changed', result['outcome']
    assert_equal 'admitted', result['phase']
    assert_match(/remote target changed/, result['error'])
    assert_equal advanced, git('-C', @remote, 'rev-parse', 'master').strip
    assert_equal @base, git('-C', @bare, 'rev-parse', 'master').strip
  end

  def test_refreshed_history_stays_in_ledger_and_worker_request_executes
    first = owner('refresh-history')
    feature(first)
    snap = @engine.prepare_merge(first, 'code', evidence(first)).merge('changeScope' => 'x' * 4000)
    snap = @engine.prepare_snapshot(first, 'merge', snap.fetch('operation'), snap.fetch('evidence'), change_scope: snap.fetch('changeScope'))
    job = @engine.mutate(first, 'submit', request('kind' => 'merge', 'dependsOn' => [], 'approvalId' => nil, 'reservationId' => nil, 'snapshot' => snap))
    70.times do
      job = @engine.mutate(first, 'refresh', request('expectedRevision' => job['revision'], 'snapshot' => snap, 'approvalId' => nil), job_id: job['id'])
    end
    job = @engine.mutate(first, 'refresh', request('expectedRevision' => job['revision'], 'snapshot' => snap, 'approvalId' => approval(first, snap)), job_id: job['id'])
    claimed = @engine.claim(owner_id: first['id']).first
    assert_equal 71, claimed['history'].length
    assert_operator JSON.generate(claimed).bytesize, :>, WorkspaceCoordination::MAX_REQUEST
    # Exercise actual dispatch publication without launching an external unit.
    @engine.define_singleton_method(:capture) { |*| '' }
    @engine.dispatch(claimed)
    path = @engine.attempt_path(claimed['attempt'], 'request.json')
    value = WorkspaceCoordination.private_read(path, limit: WorkspaceCoordination::MAX_REQUEST)
    assert_empty value['job']['history']
    assert_operator File.size(path), :<, WorkspaceCoordination::MAX_REQUEST
    WorkspaceCoordination::Worker.new(path, fixtures: true).run
    @engine.reconcile_attempts
    final = @engine.store.read['jobs'].fetch(job['id'])
    assert_equal 'succeeded', final['state']
    assert_equal 71, final['history'].length
  end

  def test_near_limit_snapshot_refuses_claim_before_worker_publication
    first = owner('large-snapshot')
    feature(first)
    snap = @engine.prepare_merge(first, 'code', evidence(first))
    payload = request('kind' => 'merge', 'dependsOn' => [], 'approvalId' => SecureRandom.uuid, 'reservationId' => nil, 'snapshot' => snap)
    extra = WorkspaceCoordination::MAX_REQUEST - JSON.generate(payload).bytesize - 1
    proofs = snap['evidence'].map(&:dup)
    proofs.first['kind'] += 'x' * extra
    snap = @engine.prepare_snapshot(first, 'merge', snap.fetch('operation'), proofs)
    payload['snapshot'], payload['approvalId'] = snap, approval(first, snap)
    assert_equal WorkspaceCoordination::MAX_REQUEST, JSON.generate(payload).bytesize + 1
    job = @engine.mutate(first, 'submit', payload)
    assert_empty @engine.claim(owner_id: first['id'])
    current = @engine.store.read['jobs'].fetch(job['id'])
    assert_equal 'needs_refresh', current['state']
    assert_equal 'worker_request_too_large', current.dig('reason', 'code')
    assert_nil current['attempt']
    assert_empty Dir[File.join(@engine.store.root, 'attempts', '*', 'request.json')]
  end

  def write_attempt(job)
    owner = @engine.store.read['owners'][job['ownerId']]
    value = {'schema' => 1, 'job' => job, 'owner' => owner, 'package' => @package, 'profile' => @profile,
      'stateRoot' => @state, 'entry' => @entry, 'workspace' => @workspace, 'catalogDigest' => @engine.catalog.digest}
    value['digest'] = WorkspaceCoordination.digest(value)
    path = @engine.attempt_path(job['attempt'], 'request.json')
    WorkspaceCoordination.atomic(path, value)
    path
  end

  def configure_machine
    command = File.join(@tmp, 'fixture-adapter')
    File.write(command, 'fixture executable')
    File.chmod(0o700, command)
    @catalog['adapters'] = {'fixture' => {'protocol' => 1, 'command' => command}}
    @catalog['machines'] = {'machine' => {'id' => 'fixture:machine', 'aliases' => ['alias'], 'adapter' => 'fixture', 'configurationRepository' => 'code', 'selector' => 'fixture'}}
    @catalog['profileMachine'] = 'fixture:machine'
    install_catalog
    @engine = engine
  end

  def deployment(owner, submit: true, coverage: [])
    artifact = @engine.save_artifact('kind' => 'profile', 'candidatePackage' => @package,
      'profileToken' => DevWorkspaceProfileIdentity.token(@profile), 'coverage' => coverage)
    snap = @engine.prepare_snapshot(owner, 'profile_activate', {'preparedArtifact' => artifact['id'],
      'candidatePackage' => @package, 'expectedProfileToken' => artifact['profileToken'], 'coverage' => coverage}, evidence(owner))
    auth = approval(owner, snap, kind: 'deploy', scope: [{'machine' => 'fixture:machine', 'actions' => ['profile_activate'], 'reboot' => false}])
    reservation = @engine.mutate(owner, 'reserve', request('target' => 'alias', 'mode' => 'deploy_first', 'approvalId' => auth, 'integrationObligations' => []))
    job = @engine.mutate(owner, 'submit', request('kind' => 'profile_activate', 'dependsOn' => [], 'approvalId' => auth, 'reservationId' => reservation['id'], 'snapshot' => snap)) if submit
    [reservation, job]
  end

  def test_review_reservation_survives_idle_then_releases_after_archive
    configure_machine
    first, second = owner('first'), owner('second')
    reservation, a = deployment(first)
    _other, b = deployment(second)
    @engine.claim
    @engine.store.transaction do |data|
      data['jobs'][a['id']]['state'] = 'succeeded'
      data['reservations'][reservation['id']]['state'] = 'deployed_review'
      job = data['jobs'][a['id']]
      data['reservations'][reservation['id']]['history'] << {'attempt' => job['attempt'], 'kind' => job['kind'],
        'artifact' => job.dig('snapshot', 'operation', 'preparedArtifact'), 'coverage' => job.dig('snapshot', 'operation', 'coverage'),
        'coverageContracts' => job['coverageContracts'], 'observed' => {'package' => @package}, 'outcome' => 'applied'}
    end
    assert_empty @engine.claim
    assert_equal 'waiting_resource', @engine.store.read['jobs'][b['id']]['state']
    assert @engine.store.session_blockers(@workspace, first['slug']).any? { |reason| reason.include?('deployed_review') }
    archive_id = 'a' * 64
    @engine.store.bind_lifecycle(@workspace, first['slug'], 'archive', archive_id)
    assert_equal 'blocked_recovery', @engine.store.read['reservations'][reservation['id']]['state']
    @engine.store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: archive_id)
    released = @engine.store.read['reservations'][reservation['id']]
    assert_equal 'released', released['state']
    @engine.store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: archive_id)
    assert_equal released, @engine.store.read['reservations'][reservation['id']]
    assert_equal [b['id']], @engine.claim.map { |job| job['id'] }
    @engine.store.revived(@workspace, first['slug'], operation_id: 'b' * 64, archive_operation_id: archive_id)
    assert_equal 'released', @engine.store.read['reservations'][reservation['id']]['state']
  end

  def test_archive_cancels_queued_deployment_and_revival_does_not_resume_it
    configure_machine
    first = owner('queued-deployer')
    reservation, job = deployment(first)
    archive_id = '9' * 64
    @engine.store.bind_lifecycle(@workspace, first['slug'], 'archive', archive_id)
    assert_equal 'canceled', @engine.store.read['jobs'][job['id']]['state']
    assert_equal 'requested', @engine.store.read['reservations'][reservation['id']]['state']
    @engine.store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: archive_id)
    @engine.store.revived(@workspace, first['slug'], operation_id: '8' * 64, archive_operation_id: archive_id)
    assert_equal 'released', @engine.store.read['reservations'][reservation['id']]['state']
    assert_equal 'canceled', @engine.store.read['jobs'][job['id']]['state']
    assert_empty @engine.claim
  end

  def test_archive_of_waiting_owner_never_claims_another_owners_machine
    configure_machine
    first, second = owner('live-deployer'), owner('waiting-deployer')
    held, = deployment(first)
    @engine.claim
    waiting, queued = deployment(second)
    archive_id = '6' * 64
    @engine.store.bind_lifecycle(@workspace, second['slug'], 'archive', archive_id)
    assert_equal 'held', @engine.store.read['reservations'][held['id']]['state']
    assert_equal 'requested', @engine.store.read['reservations'][waiting['id']]['state']
    assert_equal 'canceled', @engine.store.read['jobs'][queued['id']]['state']
    @engine.store.lifecycle_completed(@workspace, second['slug'], 'archive', operation_id: archive_id)
    assert_equal 'held', @engine.store.read['reservations'][held['id']]['state']
    assert_equal 'released', @engine.store.read['reservations'][waiting['id']]['state']
  end

  def test_archive_completion_releases_predecessor_hold_but_not_another_owner
    configure_machine
    first, second = owner('archived-holder'), owner('waiting-holder')
    reservation, job = deployment(first)
    other, = deployment(second, submit: false)
    @engine.claim
    @engine.store.transaction do |data|
      data['jobs'][job['id']]['state'] = 'succeeded'
      data['reservations'][reservation['id']]['state'] = 'deployed_review'
    end
    archive_id = 'c' * 64
    @engine.store.bind_lifecycle(@workspace, first['slug'], 'archive', archive_id)
    @engine.store.transaction do |data|
      data['lifecycles'][archive_id]['completed'] = true
      data['owners'][first['id']]['fence']['completed'] = true
      data['reservations'][reservation['id']]['state'] = 'blocked_recovery'
    end
    @engine.store.lifecycle_completed(@workspace, first['slug'], 'archive', operation_id: archive_id)
    assert_equal 'released', @engine.store.read['reservations'][reservation['id']]['state']
    assert_equal 'requested', @engine.store.read['reservations'][other['id']]['state']
  end

  def test_unknown_deployment_blocks_archive_before_releasing_ownership
    configure_machine
    first = owner('uncertain-holder')
    reservation, submitted = deployment(first)
    job = @engine.claim.first
    @engine.store.transaction do |data|
      data['jobs'][submitted['id']]['state'] = 'failed'
      data['reservations'][reservation['id']]['state'] = 'blocked_recovery'
      data['reservations'][reservation['id']]['history'] << {'attempt' => job['attempt'], 'kind' => job['kind'],
        'artifact' => job.dig('snapshot', 'operation', 'preparedArtifact'), 'coverage' => [],
        'coverageContracts' => job['coverageContracts'], 'observed' => nil, 'outcome' => 'unknown'}
    end
    assert_raises(WorkspaceCoordination::Error) { @engine.store.require_lifecycle_idle!(@workspace, first['slug']) }
    assert_raises(WorkspaceCoordination::Error) { @engine.store.bind_lifecycle(@workspace, first['slug'], 'archive', 'e' * 64) }
    assert_equal 'blocked_recovery', @engine.store.read['reservations'][reservation['id']]['state']
  end

  def test_portal_badges_report_held_states_and_archived_owners_without_mutation
    configure_machine
    first = owner('holder')
    reservation, = deployment(first, submit: false)
    stat = File.lstat(File.join(@workspace, 'work', first['slug']))
    observation = Digest::SHA256.hexdigest(JSON.generate(['session-observation-v1', @workspace, first['slug'], stat.dev, stat.ino, first['thread']]))
    %w[requested held deployed_review closing blocked_recovery released].each do |state|
      @engine.store.transaction { |data| data['reservations'][reservation['id']]['state'] = state }
      before = File.binread(File.join(@engine.store.root, 'ledger.json'))
      holders = @engine.status(portal: true)['reservationHolders']
      if %w[requested released].include?(state)
        assert_empty holders
      else
        assert_equal 1, holders.length
        assert_equal first.slice('slug', 'portalUrl').merge('ownerId' => first['id'], 'observationIdentity' => observation), holders.first.reject { |key, _| key == 'machines' }
        machine = holders.first['machines'].first
        assert_equal({'machine' => 'fixture:machine', 'state' => state}, machine.slice('machine', 'state'))
        assert_equal 'machine', machine['shortName']
        assert_equal reservation['id'], machine['id']
      end
      assert_equal before, File.binread(File.join(@engine.store.root, 'ledger.json'))
    end
    @engine.store.transaction { |data| data['reservations'][reservation['id']]['state'] = 'blocked_recovery' }
    FileUtils.mkdir_p(File.join(@workspace, 'archive'))
    File.rename(File.join(@workspace, 'work', first['slug']), File.join(@workspace, 'archive', first['slug']))
    assert_equal first['id'], @engine.status(portal: true)['reservationHolders'].first['ownerId']
    assert_equal observation, @engine.status(portal: true)['reservationHolders'].first['observationIdentity']
  end

  def test_portal_badges_are_complete_beyond_history_limit_and_group_machines
    configure_machine
    first = owner('holder')
    reservation, = deployment(first, submit: false)
    @engine.store.transaction do |data|
      129.times do |index|
        historic = first.merge('id' => format('00000000-0000-0000-0000-%012x', index), 'slug' => "historic-#{index}")
        data['owners'][historic['id']] = historic
        retained = data['reservations'][reservation['id']].merge('id' => SecureRandom.uuid,
          'ownerId' => historic['id'], 'machine' => "fixture:historic-#{index}", 'state' => 'held')
        data['reservations'][retained['id']] = retained
      end
      data['owners'].delete(first['id'])
      data['owners'][first['id']] = first
      data['reservations'][reservation['id']]['state'] = 'held'
      second = data['reservations'][reservation['id']].merge('id' => SecureRandom.uuid, 'machine' => 'fixture:another')
      data['reservations'][second['id']] = second
    end
    status = @engine.status(portal: true)
    assert_equal 128, status['owners'].length
    refute status['owners'].any? { |item| item['id'] == first['id'] }
    assert_equal 128, status['reservations'].length
    assert_equal 1, status['reservationHolders'].length
    assert_equal %w[fixture:another fixture:machine], status['reservationHolders'].first['machines'].map { |item| item['machine'] }
  end

  def test_portal_badges_do_not_transfer_to_reused_slugs_or_other_workspaces
    configure_machine
    first = owner('holder')
    reservation, = deployment(first, submit: false)
    @engine.store.transaction { |data| data['reservations'][reservation['id']]['state'] = 'held' }
    original_workspace = @workspace
    @workspace = File.join(@tmp, 'other-workspace')
    FileUtils.mkdir_p(@workspace)
    @engine = engine
    owner('holder')
    assert_empty @engine.status(portal: true)['reservationHolders']
    assert_equal 'fixture:machine', @engine.status(portal: true)['foreignReservations'].first['machine']
    @workspace = original_workspace
    @engine = engine
    replacement = owner('holder')
    refute_equal first['id'], replacement['id']
    assert_empty @engine.status(portal: true)['reservationHolders']
    assert_equal first['id'], @engine.store.read['reservations'][reservation['id']]['ownerId']
  end

  def test_lifecycle_cannot_cancel_claimed_attempt
    configure_machine
    first = owner('first')
    reservation, = deployment(first)
    @engine.claim
    assert_raises(WorkspaceCoordination::Error) { @engine.store.fence(@workspace, first['slug'], 'delete') }
    data = @engine.store.read
    assert_nil data['owners'][first['id']]['fence']
    assert_equal 'held', data['reservations'][reservation['id']]['state']
  end

  def test_absent_iteration_scope_never_authorizes_other_snapshot
    first = owner('first')
    job = submit_merge(first)
    @engine.store.transaction do |data|
      data['approvals'][data['jobs'][job['id']]['approvalId']]['reviewedSnapshot'] = 'sha256:' + ('0' * 64)
    end
    assert_empty @engine.claim
    assert_equal 'waiting_approval', @engine.store.read['jobs'][job['id']]['state']
  end

  def test_corrupt_and_unknown_ledger_never_initialize_empty_state
    @engine.store.transaction { |data| data['suspended'] = true }
    path = File.join(@engine.store.root, 'ledger.json')
    original = File.binread(path)
    File.write(path, '{"schema":1,"schema":1}')
    assert_raises(WorkspaceCoordination::Error) { @engine.store.read }
    File.write(path, original.sub('"schema":1', '"schema":99'))
    assert_raises(WorkspaceCoordination::Error) { @engine.store.transaction {} }
    assert_includes File.binread(path), '"schema":99'
  end

  def test_owner_admission_preserves_fifo_across_separate_lock_windows
    first, second = owner('first'), owner('second')
    a, = submit_merge(first), submit_merge(second)
    assert_empty @engine.claim(owner_id: second['id'])
    assert_equal [a['id']], @engine.claim(owner_id: first['id']).map { |job| job['id'] }
    assert_empty @engine.claim(owner_id: second['id'])
  end

  def test_exact_submit_retry_survives_changed_preparation_evidence
    first = owner('first')
    feature(first)
    snap = @engine.prepare_merge(first, 'code', evidence(first))
    payload = request('kind' => 'merge', 'dependsOn' => [], 'approvalId' => approval(first, snap), 'reservationId' => nil, 'snapshot' => snap)
    reply = @engine.mutate(first, 'submit', payload)
    File.write(evidence(first).first['path'], 'later review')
    assert_equal reply, @engine.mutate(first, 'submit', payload)
    assert_raises(WorkspaceCoordination::Error) { @engine.mutate(first, 'submit', payload.merge('dependsOn' => [reply['id']])) }
    assert_equal 1, @engine.store.read['jobs'].length
  end

  def test_failed_push_retains_local_merge_and_blocks_until_exact_reconciliation
    first, second = owner('first'), owner('second')
    a, b = submit_merge(first), submit_merge(second)
    hook = File.join(@remote, 'hooks/pre-receive')
    File.write(hook, "#!/bin/sh\nexit 1\n")
    File.chmod(0o700, hook)
    execute(@engine.claim.first)
    data = @engine.store.read
    assert_equal 'reconciling', data['jobs'][a['id']]['state']
    assert_equal data['jobs'][a['id']].dig('snapshot', 'operation', 'featureHead'), git('-C', @bare, 'rev-parse', 'master').strip
    assert_equal @base, git('-C', @remote, 'rev-parse', 'master').strip
    assert_empty @engine.claim
    assert_equal 'waiting_resource', @engine.store.read['jobs'][b['id']]['state']
    File.unlink(hook)
    git('-C', @bare, 'push', 'origin', 'master')
    assert_equal 'succeeded', @engine.reconcile_job(first, a['id'])['state']
  end

  def test_notification_retry_keeps_event_and_options_with_a_new_native_delivery_identity
    first = owner('first')
    submit_merge(first)
    @engine.claim
    calls = []
    @engine.define_singleton_method(:notification_call) do |recipient, event, action|
      calls << [action, Marshal.load(Marshal.dump(event)), notification_payload(recipient, event, action)]
      return {'options' => {'model' => 'saved-model', 'reasoningEffort' => 'xhigh'}} if action == 'prepare'
      raise WorkspaceCoordination::Error, 'lost native reply' if calls.count { |call| call.first == 'send' } == 1
      {'receipt' => {'threadId' => event['thread'], 'turnId' => 'observed-native-turn'}}
    end
    @engine.deliver_notifications
    data = @engine.store.read
    event = data['outbox'].values.first
    assert_equal 'retry', event['state']
    @engine.store.transaction { |ledger| ledger['outbox'][event['id']]['retryAt'] = 0 }
    @engine.deliver_notifications
    sends = calls.select { |call| call.first == 'send' }.map { |call| call[1] }
    assert_equal 2, sends.length
    assert_equal sends.first.slice('messageId', 'text', 'thread', 'actionContext', 'options', 'payloadDigest'), sends.last.slice('messageId', 'text', 'thread', 'actionContext', 'options', 'payloadDigest')
    payloads = calls.select { |call| call.first == 'send' }.map(&:last)
    assert_equal event['messageId'], payloads.first['messageId']
    refute_equal payloads.first['messageId'], payloads.last['messageId']
    assert_match WorkspaceCoordination::UUID, payloads.last['messageId']
    assert_equal payloads.last, @engine.notification_payload(first, sends.last, 'send')
    assert_equal payloads.first.reject { |key, _| key == 'messageId' }, payloads.last.reject { |key, _| key == 'messageId' }
    assert_equal 'delivered', @engine.store.read['outbox'][event['id']]['state']
    assert_equal 1, calls.count { |call| call.first == 'prepare' }
  end

  def test_admitted_jobs_settle_with_large_retained_native_policy_and_bounded_events
    first = owner('large-policy')
    original = submit_merge(first)
    template = @engine.store.read['jobs'].fetch(original['id'])
    jobs = [original['id']]
    @engine.store.transaction do |data|
      99.times do
        payload = request('snapshot' => template['snapshot'])
        @engine.store.request(data, first['id'], ['submit', nil], payload) do
          job = Marshal.load(Marshal.dump(template))
          job['id'] = SecureRandom.uuid
          job['sequence'] = @engine.store.sequence(data)
          data['jobs'][job['id']] = job
          jobs << job['id']
          @engine.reply(job, first)
        end
      end
    end
    @engine.store.transaction do |data|
      jobs.each do |id|
        job = data['jobs'].fetch(id)
        10.times do
          @engine.transition(data, job, 'ready')
          @engine.transition(data, job, 'waiting_resource', {'code' => 'execution_busy'})
        end
        @engine.transition(data, job, 'failed', {'code' => 'not_applied'})
      end
    end
    assert_equal 200, @engine.store.read['outbox'].length
    options = {'model' => 'retained-model', 'developerInstructions' => 'x' * 200_000}
    @engine.define_singleton_method(:notification_call) do |_owner, event, action|
      if action == 'prepare'
        {'options' => options.merge('event' => event['id'])}
      else
        raise 'retained native policy changed' unless artifact!(event['options'])['options'] == options.merge('event' => event['id'])
        {'receipt' => {'threadId' => event['thread'], 'turnId' => 'native-turn'}}
      end
    end
    100.times { @engine.deliver_notifications(owner_id: first['id']) }
    data = @engine.store.read
    assert data['jobs'].values.all? { |job| job['state'] == 'failed' }
    assert data['outbox'].values.all? { |event| event['state'] == 'delivered' }
    assert_operator JSON.generate(data).bytesize, :<, 1024 * 1024
    assert_equal 200, data['outbox'].values.map { |event| event['options'] }.uniq.length
  end

  def test_notification_transport_loads_and_checks_the_saved_options_artifact
    first = owner('saved-options')
    @engine.store.transaction { |data| @engine.event(data, first, {'id' => SecureRandom.uuid}, 'ready') }
    event = @engine.store.read['outbox'].values.first
    options = {'model' => 'saved-model', 'developerInstructions' => 'x' * 200_000}
    artifact = @engine.save_artifact('kind' => 'notification_options', 'options' => options)
    event['options'] = artifact['id']
    payload = nil
    @engine.define_singleton_method(:capture) do |*_argv|
      payload = JSON.parse(File.read(_argv.last))
      JSON.generate('receipt' => {'threadId' => payload['thread']})
    end
    @engine.notification_call(first, event, 'send')
    assert_equal options, payload['options']
    path = File.join(@engine.store.root, 'sources', artifact['id'].delete_prefix('sha256:'), 'artifact.json')
    File.write(path, JSON.generate(artifact.merge('options' => {'model' => 'changed'})))
    error = assert_raises(WorkspaceCoordination::Error) { @engine.notification_call(first, event, 'send') }
    assert_match(/digest changed/, error.message)
  end

  def test_prospective_admission_retains_budget_for_terminal_notification_delivery
    first = owner('capacity')
    accepted = submit_merge(first)
    @engine.store.transaction do |data|
      budget = WorkspaceCoordination::MAX_LEDGER - WorkspaceCoordination::RECOVERY_RESERVE
      chunk = {'digest' => 'sha256:' + ('0' * 64), 'result' => {'retained' => 'x' * 15_000}}
      remaining = budget - JSON.generate(data).bytesize - @engine.store.recovery_growth(data) - 8192
      (remaining / (JSON.generate(chunk).bytesize + 40)).times { data['receipts'][SecureRandom.uuid] = chunk }
      @engine.store.admission_capacity!(data)
    end
    before = @engine.store.read
    rejected = request({})
    error = assert_raises(WorkspaceCoordination::Error) do
      @engine.store.transaction do |data|
        @engine.store.request(data, first['id'], 'fixture', rejected) do
          # This record is added after the old pre-yield capacity calculation.
          2.times do
            data['receipts'][SecureRandom.uuid] = {'digest' => 'sha256:' + ('0' * 64), 'result' => {'retained' => 'y' * 15_000}}
          end
          {'accepted' => true}
        end
      end
    end
    assert_match(/admission capacity/, error.message)
    assert_equal before, @engine.store.read
    @engine.store.transaction do |data|
      @engine.transition(data, data['jobs'][accepted['id']], 'ready')
      @engine.transition(data, data['jobs'][accepted['id']], 'succeeded')
    end
    terminal = @engine.store.read
    assert_operator @engine.store.recovery_growth(terminal), :>=, 2 * (WorkspaceCoordination::MAX_RESULT - 1024)
    options = {'developerInstructions' => 'x' * 200_000}
    @engine.define_singleton_method(:notification_call) do |_owner, event, action|
      action == 'prepare' ? {'options' => options} : {'receipt' => {'turnId' => 't' * 4096}}
    end
    @engine.deliver_notifications(owner_id: first['id'])
    settled = @engine.store.read
    assert settled['outbox'].values.all? { |event| event['state'] == 'delivered' }
    assert_operator JSON.generate(settled).bytesize, :<, WorkspaceCoordination::MAX_LEDGER
  end

  def test_worker_result_and_unknown_disposition_fit_the_admitted_growth_budget
    configure_machine
    @catalog['repositories']['input'] = {'remote' => @remote, 'defaultTarget' => 'refs/heads/master'}
    install_catalog
    @engine = engine
    first = owner('uncertain-deployment')
    reservation, submitted = deployment(first, coverage: [{'repository' => 'input', 'revision' => @base}])
    job = @engine.claim(owner_id: first['id']).first
    data = @engine.store.read
    budget = JSON.generate(data).bytesize + @engine.store.recovery_growth(data)
    path = write_attempt(job)
    worker = WorkspaceCoordination::Worker.new(path, fixtures: true)
    worker.phase('profile_intent')
    observed = {'package' => @package, 'profileToken' => DevWorkspaceProfileIdentity.token(@profile), 'detail' => 'x' * 10_000}
    worker.result('unknown', observed, "\n" * 4096)
    @engine.reconcile_attempts
    unknown = @engine.store.read
    assert_equal 'reconciling', unknown['jobs'][submitted['id']]['state']
    assert_operator JSON.generate(unknown).bytesize + @engine.store.recovery_growth(unknown), :<=, budget
    worker.result('applied', observed.merge('detail' => 'y' * 10_000))
    @engine.reconcile_attempts
    settled = @engine.store.read
    assert_equal 'succeeded', settled['jobs'][submitted['id']]['state']
    assert_equal 1, settled['reservations'][reservation['id']]['history'].length
    assert_equal 1, settled['reservations'][reservation['id']]['history'].first['dispositions'].length
    assert_operator JSON.generate(settled).bytesize + @engine.store.recovery_growth(settled), :<=, budget
    assert_operator JSON.generate(WorkspaceCoordination.diagnostic("\n" * 4096)).bytesize, :<=, 4096
  end

  def test_machine_release_requires_review_and_exact_integration_without_waiting_for_notifications
    check_machine_release(:receipts)
  end

  def test_machine_release_uses_reserved_bytes_at_admission_limit
    check_machine_release(:bytes)
  end

  def check_machine_release(capacity)
    configure_machine
    first = owner('first')
    checkout = feature(first)
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    reservation, job = deployment(first)
    @engine.claim
    @engine.store.transaction do |data|
      data['jobs'][job['id']]['state'] = 'succeeded'
      data['reservations'][reservation['id']]['state'] = 'deployed_review'
      data['reservations'][reservation['id']]['history'] << {'attempt' => data['jobs'][job['id']]['attempt'], 'kind' => 'profile_activate',
        'artifact' => data['jobs'][job['id']].dig('snapshot', 'operation', 'preparedArtifact'), 'coverage' => [{'repository' => 'code', 'revision' => head}],
        'coverageContracts' => @engine.coverage_contracts(first, [{'repository' => 'code', 'revision' => head}]),
        'outcome' => 'applied', 'observed' => {'package' => @package, 'profileToken' => DevWorkspaceProfileIdentity.token(@profile)}}
    end
    value = @engine.store.read['reservations'][reservation['id']]
    payload = request('expectedRevision' => value['revision'], 'deploymentWorkFinished' => false, 'evidence' => 'review accepted',
      'finalHeads' => [{'repository' => 'code', 'target' => 'refs/heads/master', 'head' => head}])
    assert_raises(WorkspaceCoordination::Error) { @engine.finish(first, reservation['id'], payload) }
    payload['deploymentWorkFinished'] = true
    assert_raises(WorkspaceCoordination::Error) { @engine.finish(first, reservation['id'], payload) }
    git('-C', checkout, 'push', 'origin', 'HEAD:master')
    @engine.store.transaction do |data|
      data['outbox'].each_value { |event| event['state'] = 'retry'; event['attempts'] = 1; event['error'] = 'lost native reply' }
    end
    pending_events = @engine.store.read['outbox'].values
    refute_empty pending_events
    assert pending_events.all? { |event| event['state'] != 'delivered' }
    @engine.store.transaction { |data| data['owners'][first['id']]['fence'] = {'id' => SecureRandom.uuid, 'command' => 'archive', 'completed' => false, 'operationId' => nil} }
    assert_raises(WorkspaceCoordination::Error) { @engine.finish(first, reservation['id'], payload) }
    @engine.store.transaction { |data| data['owners'][first['id']]['fence'] = nil }
    fill_admission(capacity)
    assert_equal 'released', @engine.finish(first, reservation['id'], payload)['state']
    pending_events.each { |event| assert_equal event, @engine.store.read['outbox'][event['id']] }
    assert_equal 'released', @engine.finish(first, reservation['id'], payload)['state']
    fresh = payload.merge('requestId' => SecureRandom.uuid, 'expectedRevision' => @engine.store.read['reservations'][reservation['id']]['revision'])
    assert_raises(WorkspaceCoordination::Error) { @engine.finish(first, reservation['id'], fresh) }
    assert_equal [{'repository' => 'code', 'target' => 'refs/heads/master', 'head' => head, 'kind' => 'profile_activate'}], @engine.store.read['frontier']['fixture:machine']
  end

  def test_synchronous_profile_submission_is_deduplicated_without_prior_job
    configure_machine
    first = owner('first')
    reservation, = deployment(first, submit: false)
    auth = @engine.store.read['approvals'].values.first
    saved = @engine.prepared_snapshot!(first, auth['reviewedSnapshot'])
    artifact = @engine.artifact!(saved.dig('snapshot', 'operation', 'preparedArtifact'))
    children = 2.times.map do
      fork do
        @engine.find_or_submit_profile(first, artifact)
        exit! 0
      end
    end
    children.each { |pid| Process.wait(pid); assert $?.success? }
    data = @engine.store.read
    assert_equal 1, data['jobs'].length
    assert_equal reservation['id'], data['jobs'].values.first['reservationId']
    assert_equal data['jobs'].values.first['id'], @engine.find_or_submit_profile(first, artifact)['id']
    assert_raises(WorkspaceCoordination::Error) { @engine.find_or_submit_profile(first, artifact.merge('coverage' => [{'repository' => 'code', 'revision' => @base}])) }
  end

  def test_bootstrap_maintenance_loads_only_the_retained_active_incarnation
    first = owner('first')
    # The ordinary threaded fixture cannot be used as a bootstrap owner.
    assert_raises(WorkspaceCoordination::Error) { @engine.existing_bootstrap_owner(first['slug'], first['id']) }
    second = owner('second')
    path = File.join(@workspace, 'work/second/portal.yml')
    manifest = YAML.safe_load(File.read(path))
    manifest['codex'] = {}
    File.write(path, YAML.dump(manifest))
    bootstrap = @engine.owner('second', bootstrap: true)
    assert_equal bootstrap, @engine.existing_bootstrap_owner('second', bootstrap['id'])
    assert_raises(WorkspaceCoordination::Error) { @engine.existing_bootstrap_owner('first', bootstrap['id']) }
    count = @engine.store.read['owners'].length
    assert_raises(WorkspaceCoordination::Error) { @engine.existing_bootstrap_owner('second', SecureRandom.uuid) }
    original = File.join(@workspace, 'work/second')
    File.rename(original, original + '-retained')
    FileUtils.cp_r(original + '-retained', original)
    assert_raises(WorkspaceCoordination::Error) { @engine.existing_bootstrap_owner('second', bootstrap['id']) }
    assert_equal count, @engine.store.read['owners'].length
    assert_empty @engine.store.read['outbox']
  end

  def test_frontier_refuses_old_profile_coverage_but_keeps_system_surface_separate
    configure_machine
    first = owner('first')
    checkout = feature(first)
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    git('-C', checkout, 'push', 'origin', 'HEAD:master')
    @engine.store.transaction { |data| data['frontier']['fixture:machine'] = [{'repository' => 'code', 'target' => 'refs/heads/master', 'head' => head, 'kind' => 'profile_activate'}] }
    operation = {'candidatePackage' => @package, 'coverage' => [{'repository' => 'code', 'revision' => @base}]}
    assert_raises(WorkspaceCoordination::Error) { @engine.frontier_valid!(first, operation) }
    @engine.frontier_valid!(first, operation.merge('coverage' => [{'repository' => 'code', 'revision' => head}]))
    @engine.frontier_valid!(first, 'target' => 'alias', 'coverage' => [])
    # Multiple effective revisions may exist; a descendant can cover the
    # frontier even when an older selected input appears first.
    @engine.frontier_valid!(first, operation.merge('coverage' => [{'repository' => 'code', 'revision' => @base}, {'repository' => 'code', 'revision' => head}]))
  end

  def test_command_limits_drain_both_pipes_and_preserve_complete_digest
    log = WorkspaceCoordination::Command::Log.new
    program = 'STDOUT.write("x" * 400000); STDERR.write("e" * 5000000); STDOUT.write("z")'
    output, _error, status, overflow = WorkspaceCoordination::Command.run([RbConfig.ruby, '-e', program], limit: 100, log: log)
    assert status.success?
    assert overflow
    assert_operator output.bytesize, :<=, 100
    assert_operator log.bytes.bytesize, :<=, 4 * 1024 * 1024
    assert_includes log.bytes, 'diagnostics truncated'
    digest, = WorkspaceCoordination::Command.run([RbConfig.ruby, '-e', 'STDOUT.write("x" * 400000)'], digest: true)
    assert_equal 'sha256:' + Digest::SHA256.hexdigest('x' * 400000), digest
  end

  def record_profile_deployment(owner, reservation, job, coverage)
    @engine.claim
    @engine.store.transaction do |data|
      value = data['jobs'][job['id']]
      value['state'] = 'succeeded'
      data['reservations'][reservation['id']]['state'] = 'deployed_review'
      data['reservations'][reservation['id']]['history'] << {'attempt' => value['attempt'], 'kind' => 'profile_activate',
        'artifact' => value.dig('snapshot', 'operation', 'preparedArtifact'), 'coverage' => coverage,
        'coverageContracts' => @engine.coverage_contracts(owner, coverage), 'outcome' => 'applied',
        'observed' => {'package' => @package, 'profileToken' => DevWorkspaceProfileIdentity.token(@profile)}}
      data['outbox'].each_value { |event| event['state'] = 'delivered' }
    end
  end

  def finish_request(reservation, head)
    value = @engine.store.read['reservations'][reservation['id']]
    request('expectedRevision' => value['revision'], 'deploymentWorkFinished' => true, 'evidence' => 'UI review accepted',
      'finalHeads' => [{'repository' => 'code', 'target' => 'refs/heads/master', 'head' => head}])
  end

  def test_unregistered_integrated_input_is_proved_without_an_invented_feature
    configure_machine
    @catalog['repositories']['input'] = {'remote' => @remote, 'defaultTarget' => 'refs/heads/master'}
    git('clone', '--bare', @remote, File.join(@workspace, 'repos/input.git'))
    install_catalog
    @engine = engine
    first = owner('first')
    checkout = feature(first)
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    coverage = [{'repository' => 'code', 'revision' => head}, {'repository' => 'input', 'revision' => @base}]
    reservation, job = deployment(first, coverage: coverage)
    record_profile_deployment(first, reservation, job, coverage)
    git('-C', checkout, 'push', 'origin', 'HEAD:master')
    assert_equal 'released', @engine.finish(first, reservation['id'], finish_request(reservation, head))['state']
    value = @engine.store.read['reservations'][reservation['id']]
    input = value['completion']['integrationProofs'].find { |proof| proof['repository'] == 'input' }
    assert_equal false, input['owned']
    assert_equal @base, input['head']
    assert_equal head, input['remoteHead']
    assert @engine.store.read['frontier']['fixture:machine'].any? { |proof| proof['repository'] == 'input' && proof['head'] == @base }
  end

  def test_reviewed_equivalent_rebase_releases_without_erasing_deployed_history
    configure_machine
    first = owner('first')
    checkout = feature(first)
    before = git('-C', checkout, 'rev-parse', 'HEAD').strip
    snap = @engine.prepare_merge(first, 'code', evidence(first))
    auth = approval(first, snap)
    coverage = [{'repository' => 'code', 'revision' => before}]
    reservation, job = deployment(first, coverage: coverage)
    record_profile_deployment(first, reservation, job, coverage)
    upstream = File.join(@tmp, 'upstream')
    git('clone', @remote, upstream)
    git('-C', upstream, 'config', 'user.name', 'Fixture')
    git('-C', upstream, 'config', 'user.email', 'fixture@example.test')
    File.write(File.join(upstream, 'independent'), 'independent upstream work')
    git('-C', upstream, 'add', 'independent')
    git('-C', upstream, 'commit', '-m', 'fixture upstream')
    git('-C', upstream, 'push', 'origin', 'master')
    git('-C', checkout, 'fetch', 'origin', 'master')
    new_base = git('-C', checkout, 'rev-parse', 'FETCH_HEAD').strip
    git('-C', checkout, 'rebase', new_base)
    after = git('-C', checkout, 'rev-parse', 'HEAD').strip
    refute_equal before, after
    git('-C', checkout, 'push', 'origin', 'HEAD:master')
    payload = finish_request(reservation, after)
    assert_raises(WorkspaceCoordination::Error) { @engine.finish(first, reservation['id'], payload) }
    directory = File.join(@workspace, 'work', first['slug'])
    range_path = File.join(directory, 'range.diff')
    File.write(range_path, git('-C', checkout, 'range-diff', '--no-color', '--creation-factor=80', "#{@base}..#{before}", "#{new_base}..#{after}"))
    mapping = {'repository' => 'code', 'target' => 'refs/heads/master', 'from' => before, 'fromBase' => @base,
      'to' => after, 'toBase' => new_base, 'relation' => 'patch_equivalent', 'approvalId' => auth}
    artifacts = %w[range_diff verification migration_review].map do |kind|
      path = kind == 'range_diff' ? range_path : evidence(first).first['path']
      {'kind' => kind, 'path' => path, 'sha256' => Digest::SHA256.file(path).hexdigest}
    end
    receipt_path = File.join(directory, 'lineage.json')
    File.write(receipt_path, JSON.generate('schema' => 1, 'ownerId' => first['id'], 'reservationId' => reservation['id'],
      'mapping' => mapping, 'decision' => 'accepted', 'reviewer' => 'independent fixture reviewer', 'artifacts' => artifacts))
    proof = {'kind' => 'review', 'path' => receipt_path, 'sha256' => Digest::SHA256.file(receipt_path).hexdigest}
    payload['replacements'] = [mapping.merge('evidence' => proof)]
    assert_equal 'released', @engine.finish(first, reservation['id'], payload)['state']
    value = @engine.store.read['reservations'][reservation['id']]
    assert_equal before, value['history'].first['coverage'].first['revision']
    assert_equal after, @engine.store.read['frontier']['fixture:machine'].first['head']
    assert File.directory?(File.join(@engine.store.root, 'sources', before))
  end
end

class WorkspaceCoordinationTest < Minitest::Test
  def test_candidate_profile_recovery_retains_original_attempt_and_claim
    configure_machine
    first = owner('managed-candidate')
    candidate = File.join(@tmp, 'new-candidate')
    FileUtils.mkdir_p(File.join(candidate, 'bin'))
    artifact = @engine.save_artifact('kind' => 'profile', 'candidatePackage' => candidate,
      'profileToken' => DevWorkspaceProfileIdentity.token(@profile), 'coverage' => [])
    snapshot = @engine.prepare_snapshot(first, 'profile_activate', {
      'preparedArtifact' => artifact['id'], 'candidatePackage' => candidate,
      'expectedProfileToken' => artifact['profileToken'], 'coverage' => []}, evidence(first))
    auth = approval(first, snapshot, kind: 'deploy', scope: [{'machine' => 'fixture:machine', 'actions' => ['profile_activate'], 'reboot' => false}])
    reservation = @engine.mutate(first, 'reserve', request('target' => 'alias', 'mode' => 'deploy_first', 'approvalId' => auth, 'integrationObligations' => []))
    submitted = @engine.mutate(first, 'submit', request('kind' => 'profile_activate', 'dependsOn' => [], 'approvalId' => auth, 'reservationId' => reservation['id'], 'snapshot' => snapshot))
    job = @engine.claim(owner_id: first['id']).first
    worker = WorkspaceCoordination::Worker.new(write_attempt(job), fixtures: true)
    worker.phase('profile_intent')
    worker.result('unknown', nil, 'predecessor refused new registration argv before selection')
    @engine.reconcile_attempts
    assert_equal 'reconciling', @engine.store.read['jobs'][submitted['id']]['state']
    assert_equal 'blocked_recovery', @engine.store.read['reservations'][reservation['id']]['state']
    # Exercise the actual child activation claim guard, rather than mocking
    # the entire candidate invocation and missing the recovery reservation.
    host = DevWorkspaceHost::Host.allocate
    host.instance_variable_set(:@env, {})
    engine = @engine
    host.define_singleton_method(:coordination_store) { engine.store }
    host.define_singleton_method(:coordination_catalog) { engine.catalog }
    host.instance_variable_set(:@coordination_attempt, job['attempt'])
    host.send(:require_coordination_profile_claim!, candidate)
    assert_raises(WorkspaceCoordination::Error) { host.send(:require_coordination_profile_claim!, '/wrong-candidate') }
    @engine.store.transaction { |data| data['reservations'][reservation['id']]['history'][0]['artifact'] = 'sha256:' + ('f' * 64) }
    assert_raises(WorkspaceCoordination::Error) { host.send(:require_coordination_profile_claim!, candidate) }
    @engine.store.transaction { |data| data['reservations'][reservation['id']]['history'][0]['artifact'] = artifact['id'] }
    # A wrong candidate cannot recover an uncertain original intent.
    assert_raises(WorkspaceCoordination::Error) { @engine.reconcile_job(first, job['id'], candidate_host: '/wrong/workspace-host') }
    calls = []
    original_capture = @engine.method(:capture)
    profile = @profile
    @engine.define_singleton_method(:capture) do |*argv, **options|
      if argv.first == File.join(candidate, 'bin/workspace-host')
        host.send(:require_coordination_profile_claim!, candidate)
        calls << argv
        File.unlink(profile)
        File.symlink(candidate, profile)
        ''
      else
        original_capture.call(*argv, **options)
      end
    end
    result = @engine.reconcile_job(first, job['id'], candidate_host: File.join(candidate, 'bin/workspace-host'))
    assert_equal 'succeeded', result['state']
    assert_equal [[File.join(candidate, 'bin/workspace-host'), 'activate-prepared', '--artifact', artifact['id'], '--attempt', job['attempt']]], calls
    data = @engine.store.read
    assert_equal job['attempt'], data['jobs'][job['id']]['attempt']
    assert_equal first['id'], data['reservations'][reservation['id']]['ownerId']
    assert_equal 'deployed_review', data['reservations'][reservation['id']]['state']
    assert_equal 1, data['reservations'][reservation['id']]['history'].length
    assert_equal 'unknown', data['reservations'][reservation['id']]['history'][0]['dispositions'][0]['outcome']
  end
end
