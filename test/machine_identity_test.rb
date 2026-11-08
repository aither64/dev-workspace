# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require 'rbconfig'
require_relative '../libexec/workspace-coordination-execution'
require_relative '../libexec/workspace-coordination-claims-host'
require_relative 'support/coordination_fixture'

class MachineIdentityTest < Minitest::Test
  include CoordinationFixture

  def setup
    super
    @origin = 'github.com/example/configuration'
    remote = 'git@github.com:Example/Configuration.git'
    git('-C', @bare, 'remote', 'set-url', 'origin', remote)
    @catalog['repositories']['code']['remote'] = remote
    seed = File.join(@tmp, 'machine-base')
    git('-C', @bare, 'worktree', 'add', '-b', 'machine-base', seed, @base)
    File.write(File.join(seed, 'flake.nix'), '{ outputs = _: {}; }')
    git('-C', seed, 'config', 'user.name', 'Fixture')
    git('-C', seed, 'config', 'user.email', 'fixture@example.test')
    git('-C', seed, 'add', 'flake.nix')
    git('-C', seed, 'commit', '-m', 'fixture flake')
    @base = git('-C', seed, 'rev-parse', 'HEAD').strip
    @resolver_state, @resolver_calls = File.join(@tmp, 'resolver.json'), File.join(@tmp, 'resolver-calls')
    File.write(@resolver_state, JSON.generate('label' => nil))
    adapter = File.join(@tmp, 'resolver')
    File.write(adapter, <<~RUBY)
      #!#{RbConfig.ruby}
      require 'json'
      request = JSON.parse(STDIN.read)
      state = JSON.parse(File.read(#{@resolver_state.inspect}))
      File.open(#{File.join(@state, 'coordination', 'ledger.lock').inspect}, File::RDWR | File::CREAT, 0600) do |lock|
        abort 'resolver ran under ledger mutex' unless lock.flock(File::LOCK_EX | File::LOCK_NB)
      end
      File.open(#{@resolver_calls.inspect}, 'a') { |file| file.puts(JSON.generate(request)) }
      abort 'wrong resolver request' unless request.keys.sort == %w[operation schema selector source] && request['operation'] == 'resolve-machine' && request['schema'] == 1 && request['source'].keys.sort == %w[path revision]
      if state['changeRef']
        system('git', '-C', request['source']['path'], 'update-ref', state['changeRef'], state.fetch('revision')) || abort('ref update failed')
      end
      if state['raw']
        STDOUT.write(state['raw'])
      else
        puts JSON.generate('schema' => 1, 'name' => state.fetch('name', request['selector']), 'sharedLockId' => state['label'])
      end
    RUBY
    File.chmod(0o700, adapter)
    @catalog['adapters']['confctl'] = {'protocol' => 1, 'command' => adapter}
    install_catalog
    File.write(File.join(@package, 'share/dev-workspace/claim-contract.json'), JSON.generate(WorkspaceCoordination::CLAIM_CONTRACT))
    File.write(File.join(@package, 'share/dev-workspace/machine-claim-contract.json'), JSON.generate(WorkspaceCoordination::MACHINE_CLAIM_CONTRACT))
    @engine = engine
  end

  def person(slug)
    value = owner(slug)
    feature(value)
    value
  end

  def typed(owner, selector: 'vps', repository: 'code', adapter: 'confctl', id: SecureRandom.uuid)
    @engine.acquire_claim(owner, {'schema' => 2, 'requestId' => id, 'label' => 'configured machine', 'repositories' => [],
      'machines' => [{'repository' => repository, 'selector' => selector, 'adapter' => adapter}]})
  end

  def resolver(**values)
    File.write(@resolver_state, JSON.generate(values.transform_keys(&:to_s)))
  end

  def policy(aliases: [], profile: nil)
    path = File.join(@package, 'share/dev-workspace/machine-identity-policy.json')
    File.write(path, JSON.generate('schema' => 1, 'profileTarget' => profile, 'legacyAliases' => aliases))
    @engine = engine
  end

  def legacy_machine
    @catalog['machines']['host'] = {'id' => 'old:host', 'aliases' => ['alias'], 'configurationRepository' => 'code', 'selector' => 'vps', 'adapter' => 'confctl'}
    @catalog['profileMachine'] = 'host'
    install_catalog
    @engine = engine
  end

  def test_fixed_key_and_display_vectors_ignore_object_key_order
    identity = {'origin' => 'github.com/example/configuration', 'name' => 'region/machines/development', 'kind' => 'repository'}
    expected = 'machine:logical:v1:5d7b9846f8019fea13e81daa5cd4ae6a65c2c1fb79aed4eba1a7d4eea1b6848d'
    assert_equal(expected, WorkspaceCoordination::MachineIdentity.resource(identity))
    assert_equal(expected, WorkspaceCoordination::MachineIdentity.resource(identity.to_a.reverse.to_h))
    assert_equal('machine:github.com/example/configuration:region/machines/development', WorkspaceCoordination::MachineIdentity.identifier(identity))
    assert_equal('machine:shared:a%3Ab%25/%C5%BE', WorkspaceCoordination::MachineIdentity.identifier('kind' => 'shared', 'id' => 'a:b%/ž'))
    ['', ' x', "x\n", "x\u0085", "x\u0000", "\xff".b, 'x' * 257].each do |bad|
      assert_raises(WorkspaceCoordination::Error) { WorkspaceCoordination::MachineIdentity.text!(bad) }
    end
    assert_equal('Ž:東', WorkspaceCoordination::MachineIdentity.text!('Ž:東'))
  end

  def test_aliases_clones_and_branches_share_one_machine_without_repository_claims
    first, second = person('first'), person('second')
    clone = File.join(@workspace, 'repos', 'alias.git')
    git('clone', '--bare', @bare, clone)
    git('-C', clone, 'remote', 'set-url', 'origin', 'ssh://git@github.com/example/configuration.git')
    path = File.join(@workspace, 'work', second['slug'], 'portal.yml')
    manifest = YAML.safe_load(File.read(path))
    manifest['repositories'] << {'name' => 'alias', 'project' => 'alias', 'branch' => second['slug'], 'default_branch' => 'master'}
    File.write(path, YAML.dump(manifest))
    held = typed(first)
    waiting = typed(second, repository: 'alias')
    assert_equal('held', held['state'])
    assert_equal('waiting', waiting['state'])
    assert_equal(held['resources'], waiting['resources'])
    assert_equal(2, @engine.store.claims['schema'])
    assert_equal([], held['resources'].grep(/^(git:|repository:)/))
    assert_empty(Dir.glob(File.join(@engine.store.root, 'sources', '*')))
    source = JSON.parse(File.readlines(@resolver_calls).first).fetch('source')
    assert_equal(@bare, source['path'])
    assert_equal(held['machineBindings'].first['sourceRevision'], source['revision'])
    assert_equal(git('-C', @bare, 'rev-parse', first['slug']).strip, held['machineBindings'].first['sourceRevision'])
  end

  def test_released_exact_retry_precedes_missing_clone_and_retains_schema_two_gate
    owner = person('history')
    claim = typed(owner)
    released = @engine.change_claim(owner, claim['id'], 'release')
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    File.rename(@bare, @bare + '.retained')
    assert_equal(released, @engine.acquire_claim(owner, claim['request']))
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
    contract = File.join(@package, 'share/dev-workspace/machine-claim-contract.json')
    File.unlink(contract)
    assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(@package) }.code)
    File.write(contract, JSON.generate(WorkspaceCoordination::MACHINE_CLAIM_CONTRACT))
    @engine.store.require_claim_capability!(@package)
  end

  def test_active_source_pair_drift_refuses_new_insertion_and_waiting_grant
    first, second, third = person('first'), person('second'), person('third')
    held, waiting = typed(first), typed(second)
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    [first, third].each do |owner|
      resolver(label: 'shared-next')
      assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(owner) }.code)
      assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
    end
    @engine.change_claim(first, held['id'], 'release')
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { @engine.tick_claims(second) }.code)
    assert_equal('waiting', @engine.store.claims['claims'][waiting['id']]['state'])
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(third) }.code)
    @engine.change_claim(second, waiting['id'], 'cancel')
    assert_equal('held', typed(third)['state'], 'terminal history must not freeze future mapping choices')
  end

  def test_changed_or_removed_shared_label_cannot_bypass_a_waiter
    ['different-shared', nil].each_with_index do |changed, index|
      resolver(label: 'original-shared')
      first, second, third = person("first#{index}"), person("second#{index}"), person("third#{index}")
      held, waiting = typed(first), typed(second)
      @engine.change_claim(first, held['id'], 'release')
      resolver(label: changed)
      before = File.binread(File.join(@engine.store.root, 'claims.json'))
      assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(third) }.code)
      assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
      @engine.change_claim(second, waiting['id'], 'cancel')
      fresh = typed(third)
      assert_equal('held', fresh['state'])
      @engine.change_claim(third, fresh['id'], 'release')
    end
  end

  def test_adapter_id_drift_refuses_but_same_id_package_update_contends
    first, second = person('first'), person('second')
    held = typed(first)
    @catalog['adapters']['another'] = @catalog['adapters']['confctl'].dup
    install_catalog
    @engine = engine
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(second, adapter: 'another') }.code)
    replacement = File.join(@tmp, 'updated-resolver')
    FileUtils.cp(@catalog['adapters']['confctl']['command'], replacement)
    @catalog['adapters']['confctl']['command'] = replacement
    install_catalog
    @engine = engine
    assert_equal('waiting', typed(second)['state'])
    assert_equal('held', @engine.store.claims['claims'][held['id']]['state'])
  end

  def test_bridge_restores_contention_without_rebinding_held_source
    first, second = person('first'), person('second')
    held = typed(first)
    identity = {'kind' => 'shared', 'id' => 'deliberate-shared'}
    resolver(label: identity['id'])
    policy(aliases: [{'resource' => held['resources'].first, 'identity' => identity}])
    assert_equal('waiting', typed(second)['state'])
    @catalog['adapters']['different'] = @catalog['adapters']['confctl'].dup
    install_catalog
    @engine = engine
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(second, adapter: 'different') }.code)
    assert_equal(held['machineBindings'], @engine.store.claims['claims'][held['id']]['machineBindings'])
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { @engine.verify_machine(first, held['id'], 'code', 'vps') }.code)
  end

  def test_source_ref_change_and_missing_feature_never_fall_back_to_master
    owner = person('source')
    resolver(changeRef: "refs/heads/#{owner['slug']}", revision: @base)
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(owner) }.code)
    assert_empty(@engine.store.claims['claims'])
    git('-C', @bare, 'update-ref', '-d', "refs/heads/#{owner['slug']}")
    resolver(label: nil)
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(owner) }.code)
    assert_empty(@engine.store.claims['claims'])
  end

  def test_verify_is_read_only_allows_new_same_key_commit_and_packed_refs
    owner = person('verify')
    held = typed(owner)
    checkout = File.join(@tmp, owner['slug'])
    File.write(File.join(checkout, 'next'), 'same logical target')
    git('-C', checkout, 'add', 'next')
    git('-C', checkout, 'commit', '-m', 'fixture next source')
    git('-C', @bare, 'pack-refs', '--all')
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    result = @engine.verify_machine(owner, held['id'], 'code', 'vps')
    assert_equal(git('-C', checkout, 'rev-parse', 'HEAD').strip, result['sourceRevision'])
    refute_equal(held['machineBindings'].first['sourceRevision'], result['sourceRevision'])
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
  end

  def test_waiting_claim_grants_with_large_packed_ref_inventory
    first, second = person('holder'), person('waiter')
    held, waiting = typed(first), typed(second)
    assert_equal('waiting', waiting['state'])
    git('-C', @bare, 'pack-refs', '--all')
    packed = File.join(@bare, 'packed-refs')
    # Archive retention adds many refs; long unrelated records can cross read
    # chunks. Put both before the wanted branch, with a substring lookalike.
    original = File.binread(packed)
    refs = (0...5000).map { |i| "#{@base} refs/archive-retention/#{i.to_s.rjust(8, '0')}\n" }.join
    refs << "#{@base} refs/archive-retention/#{'segment/' * 200}end\n"
    refs << "#{@base} refs/heads/#{second['slug']}-extra\n"
    File.binwrite(packed, "# pack-refs with: peeled\n" + refs + original.lines.reject { |line| line.start_with?('#') }.join)
    assert_operator(File.size(packed), :>, WorkspaceCoordination::MAX_REQUEST)
    refute_path_exists(File.join(@bare, 'refs', 'heads', second['slug']))

    @engine.change_claim(first, held['id'], 'release')
    @engine.tick_claims(second)
    assert_equal('held', @engine.store.claims['claims'][waiting['id']]['state'])
    assert_equal(git('-C', @bare, 'rev-parse', "refs/heads/#{second['slug']}").strip,
      @engine.verify_machine(second, waiting['id'], 'code', 'vps')['sourceRevision'])
  end

  def test_malformed_responses_and_duplicate_identities_never_insert
    owner = person('invalid')
    ['{"schema":1,"name":"vps","sharedLockId":null,"extra":1}', '{"schema":1,"name":"vps","name":"vps","sharedLockId":null}', '{"schema":1,"name":"different","sharedLockId":null}', '{"schema":1,"name":"vps","sharedLockId":" "}'].each do |raw|
      resolver(raw:)
      assert_raises(WorkspaceCoordination::Error) { typed(owner) }
      assert_empty(@engine.store.claims['claims'])
    end
    resolver(label: nil)
    input = {'schema' => 2, 'requestId' => SecureRandom.uuid, 'label' => 'duplicate', 'repositories' => [],
      'machines' => [{'repository' => 'code', 'selector' => 'vps', 'adapter' => 'confctl'}] * 2}
    assert_raises(WorkspaceCoordination::Error) { @engine.acquire_claim(owner, input) }
  end

  def test_legacy_accepted_alias_restricts_drift_even_without_held_claim
    first, second = person('first'), person('second')
    @catalog['machines']['host'] = {'id' => 'old:host', 'aliases' => [], 'configurationRepository' => 'code', 'selector' => 'vps', 'adapter' => 'confctl'}
    install_catalog
    @engine = engine
    resolver(label: 'shared-target')
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { typed(first) }.code)
    shared = {'kind' => 'shared', 'id' => 'shared-target'}
    policy(aliases: [{'resource' => 'machine:old:host', 'identity' => shared}])
    held = typed(first)
    legacy = @engine.acquire_claim(second, request('repositories' => [], 'machines' => ['host'], 'label' => 'legacy'))
    assert_equal('waiting', legacy['state'])
    @engine.change_claim(first, held['id'], 'release')
    @engine.tick_claims(second)
    assert_equal('held', @engine.store.claims['claims'][legacy['id']]['state'])
  end

  def test_schema_upgrade_preserves_legacy_rows_and_strict_typed_bindings
    owner = person('upgrade')
    old = @engine.acquire_claim(owner, request('repositories' => ['code'], 'machines' => [], 'label' => 'legacy'))
    old_row = @engine.store.claims['claims'][old['id']]
    assert_equal(1, @engine.store.claims['schema'])
    held = typed(owner)
    saved = @engine.store.claims
    assert_equal(2, saved['schema'])
    assert_equal(old_row, saved['claims'][old['id']])
    path = File.join(@engine.store.root, 'claims.json')
    bytes = File.binread(path)
    [%w[extra field], %w[sourceRevision missing], %w[identity missing]].each do |key, value|
      invalid = Marshal.load(Marshal.dump(saved))
      invalid['claims'][held['id']]['machineBindings'].first[key] = value
      assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.write_claims(invalid) }.code)
      assert_equal(bytes, File.binread(path))
    end
    saved['schema'] = 1
    assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.write_claims(saved) }.code)
    assert_equal(bytes, File.binread(path))
  end

  def test_terminal_capability_is_strict_and_policy_cycles_refuse
    owner = person('capability')
    held = typed(owner)
    @engine.change_claim(owner, held['id'], 'release')
    path = File.join(@package, 'share/dev-workspace/machine-claim-contract.json')
    ['schema', 'machineIdentityVersion', 'resolverProtocol', 'claimRecordSchemas'].each do |field|
      contract = WorkspaceCoordination::MACHINE_CLAIM_CONTRACT.merge(field => field == 'claimRecordSchemas' ? [1.0, 2] : 1.0)
      File.write(path, JSON.generate(contract))
      assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(@package) }.code)
    end
    identity = {'kind' => 'shared', 'id' => 'cycle'}
    assert_equal(5, assert_raises(WorkspaceCoordination::Error) { policy(aliases: [{'resource' => WorkspaceCoordination::MachineIdentity.resource(identity), 'identity' => identity}]) }.code)
  end

  def generic_machine_candidate
    candidate = File.join(@tmp, 'generic-machine-candidate')
    share = File.join(candidate, 'share/dev-workspace')
    FileUtils.mkdir_p(share)
    File.write(File.join(share, 'coordination-contract.json'), JSON.generate(WorkspaceCoordination::CONTRACT))
    File.write(File.join(share, 'claim-contract.json'), JSON.generate(WorkspaceCoordination::CLAIM_CONTRACT))
    candidate
  end

  def test_unreadable_retained_root_cannot_hide_terminal_or_empty_typed_history
    skip 'permission fixture requires an ordinary uid' if Process.euid.zero?
    owner = person('unreadable-history')
    claim = typed(owner)
    @engine.change_claim(owner, claim['id'], 'release')
    candidate = generic_machine_candidate
    terminal = @engine.store.claims
    [terminal, {'schema' => 2, 'claims' => {}}].each do |value|
      @engine.store.write_claims(value)
      paths = %w[claims.json ledger.json].map { |name| File.join(@engine.store.root, name) }
      before = paths.to_h { |path| [path, File.binread(path)] }
      assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.require_compatible!(candidate) }.code)
      [@engine.store.root, @state].each do |unreadable|
        File.chmod(0o000, unreadable)
        begin
          [-> { @engine.store.claims }, -> { @engine.store.read },
           -> { @engine.store.require_compatible!(candidate) }].each do |operation|
            error = assert_raises(WorkspaceCoordination::Error, &operation)
            assert_equal(5, error.code)
          end
        ensure
          File.chmod(0o700, unreadable)
        end
      end
      before.each { |path, bytes| assert_equal(bytes, File.binread(path)) }
    end
  end

  def test_retained_file_lstat_and_read_errors_refuse_generic_candidates
    candidate = generic_machine_candidate
    @engine.store.write_claims({'schema' => 2, 'claims' => {}})
    @engine.store.transaction { |ledger| ledger['suspended'] = true }
    paths = %w[claims.json ledger.json].map { |name| File.join(@engine.store.root, name) }
    before = paths.to_h { |path| [path, File.binread(path)] }
    [:lstat, :binread].product(paths, [Errno::EACCES, Errno::EIO]).each do |method, unreadable, failure|
      original = File.method(method)
      File.define_singleton_method(method) do |path, *arguments, **options|
        raise failure, path if path == unreadable
        original.call(path, *arguments, **options)
      end
      begin
        reader = unreadable.end_with?('claims.json') ? -> { @engine.store.claims } : -> { @engine.store.read }
        [reader, -> { @engine.store.require_compatible!(candidate) }].each do |operation|
          error = assert_raises(WorkspaceCoordination::Error, &operation)
          assert_equal(5, error.code)
        end
      ensure
        File.define_singleton_method(method, original)
      end
      before.each { |path, bytes| assert_equal(bytes, File.binread(path)) }
    end
  end

  def test_genuine_absence_stays_compatible_but_present_invalid_or_symlink_state_refuses
    candidate = generic_machine_candidate
    assert_empty(@engine.store.claims['claims'])
    assert_equal(@engine.store.empty, @engine.store.read)
    @engine.store.require_compatible!(candidate)
    FileUtils.mkdir_p(@engine.store.root, mode: 0o700)
    %w[claims.json ledger.json].each do |name|
      path = File.join(@engine.store.root, name)
      ['null', '{malformed'].each do |payload|
        File.write(path, payload, mode: 'w', perm: 0o600)
        error = assert_raises(WorkspaceCoordination::Error) { @engine.store.require_compatible!(candidate) }
        assert_equal(5, error.code) if name == 'claims.json'
        assert_equal(payload, File.binread(path))
      end
      File.unlink(path)
      File.symlink(File.join(@tmp, 'missing-target'), path)
      assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.require_compatible!(candidate) }.code)
      assert(File.symlink?(path))
      File.unlink(path)
    end
  end

  def test_waiting_new_commit_keeps_original_audit_revision_and_fifo_across_legacy_alias
    first, second, third = person('first'), person('second'), person('third')
    legacy_machine
    held = typed(first)
    old = @engine.acquire_claim(second, request('repositories' => [], 'machines' => ['alias'], 'label' => 'older legacy waiter'))
    later = typed(third)
    original = later['machineBindings'].first['sourceRevision']
    checkout = File.join(@tmp, third['slug'])
    File.write(File.join(checkout, 'next'), 'same identity')
    git('-C', checkout, 'add', 'next')
    git('-C', checkout, 'commit', '-m', 'fixture later commit')
    @engine.change_claim(first, held['id'], 'release')
    @engine.tick_claims(third)
    assert_equal('waiting', @engine.store.claims['claims'][later['id']]['state'])
    @engine.tick_claims(second)
    assert_equal('held', @engine.store.claims['claims'][old['id']]['state'])
    assert_equal(['machine:old:host'], @engine.store.claims['claims'][old['id']]['resources'])
    @engine.change_claim(second, old['id'], 'release')
    @engine.tick_claims(third)
    assert_equal('held', @engine.store.claims['claims'][later['id']]['state'])
    assert_equal(original, @engine.store.claims['claims'][later['id']]['machineBindings'].first['sourceRevision'])
  end

  def test_legacy_jobs_and_reservations_contend_in_both_directions_without_rewriting
    first, second = person('modern'), person('legacy')
    legacy_machine
    held = typed(first)
    artifact = @engine.save_artifact('kind' => 'profile', 'candidatePackage' => @package, 'profileToken' => DevWorkspaceProfileIdentity.token(@profile), 'coverage' => [])
    snapshot = @engine.prepare_snapshot(second, 'profile_activate', {'preparedArtifact' => artifact['id'], 'candidatePackage' => @package,
      'expectedProfileToken' => artifact['profileToken'], 'coverage' => []}, evidence(second))
    auth = approval(second, snapshot, kind: 'deploy', scope: [{'machine' => 'old:host', 'actions' => ['profile_activate'], 'reboot' => false}])
    reservation = @engine.mutate(second, 'reserve', request('target' => 'host', 'mode' => 'deploy_first', 'approvalId' => auth, 'integrationObligations' => []))
    job = @engine.mutate(second, 'submit', request('kind' => 'profile_activate', 'dependsOn' => [], 'approvalId' => auth, 'reservationId' => reservation['id'], 'snapshot' => snapshot))
    assert_empty(@engine.claim(owner_id: second['id']))
    @engine.change_claim(first, held['id'], 'release')
    assert_equal(job['id'], @engine.claim(owner_id: second['id']).first['id'])
    waiter = typed(first)
    assert_equal('waiting', waiter['state'])
    current = @engine.store.read
    assert_equal('old:host', current['reservations'][reservation['id']]['machine'])
    assert_equal(snapshot, current['jobs'][job['id']]['snapshot'])
    @engine.store.transaction do |ledger|
      @engine.transition(ledger, ledger['jobs'][job['id']], 'failed')
      ledger['reservations'][reservation['id']]['state'] = 'released'
    end
    @engine.tick_claims(first)
    assert_equal('held', @engine.store.claims['claims'][waiter['id']]['state'])
  end

  def test_other_workspaces_and_repositories_use_portable_identity_and_shared_labels
    [[nil, 'example/configuration'], [nil, 'another/configuration'], ['intentional-overlap', 'another/configuration']].each_with_index do |(label, remote), index|
      resolver(label:)
      first = person("first#{index}")
      held = typed(first)
      other_workspace = File.join(@tmp, "other-workspace-#{index}")
      clone = File.join(other_workspace, 'repos', 'client.git')
      FileUtils.mkdir_p(File.dirname(clone))
      git('clone', '--bare', @bare, clone)
      git('-C', clone, 'remote', 'set-url', 'origin', "git@github.com:#{remote}.git")
      git('-C', clone, 'update-ref', 'refs/heads/other', @base)
      directory = File.join(other_workspace, 'work', 'other')
      FileUtils.mkdir_p(directory)
      manifest = {'slug' => 'other', 'repositories' => [{'name' => 'client', 'project' => 'client', 'branch' => 'other', 'default_branch' => 'master'}],
        'codex' => {'thread_id' => SecureRandom.uuid, 'socket_path' => @entry['socket']}}
      File.write(File.join(directory, 'portal.yml'), YAML.dump(manifest))
      File.write(File.join(directory, 'state.md'), "---\nlifecycle: active\n---\n")
      other = WorkspaceCoordination::Engine.new(state: @state, package: @package, profile: @profile, workspace: other_workspace, entry: @entry, fixtures: true)
      owner = other.owner('other')
      input = held['request'].merge('requestId' => SecureRandom.uuid, 'machines' => [{'repository' => 'client', 'selector' => 'vps', 'adapter' => 'confctl'}])
      claim = other.acquire_claim(owner, input)
      same = label != nil || remote == 'example/configuration'
      assert_equal(same ? 'waiting' : 'held', claim['state'])
      assert_equal(same, held['resources'] == claim['resources'])
      @engine.change_claim(first, held['id'], 'release')
      other.change_claim(owner, claim['id'], same ? 'cancel' : 'release')
    end
  end

  def test_profile_mapping_refuses_and_projection_keeps_safe_string_arrays
    first, second = person('first'), person('second')
    legacy_machine
    held = typed(first)
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    candidate = File.join(@tmp, 'candidate')
    FileUtils.cp_r(@package, candidate)
    identity = {'kind' => 'shared', 'id' => 'different-profile'}
    File.write(File.join(candidate, 'share/dev-workspace/machine-identity-policy.json'), JSON.generate('schema' => 1, 'profileTarget' => identity, 'legacyAliases' => []))
    assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(candidate) }.code)
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
    @engine.change_claim(first, held['id'], 'release')
    resolver(label: 'one')
    # No legacy alias accepts the changed pair in this independent policy case.
    @catalog['machines'], @catalog['profileMachine'] = {}, nil
    install_catalog
    @engine = engine
    a = typed(first)
    b = typed(second, selector: 'other')
    assert_equal('waiting', b['state'])
    assert_equal('machine:shared:one', @engine.status(first['slug'], portal: true)['claims'].find { |item| item['id'] == a['id'] }['machines'].first)
    assert_kind_of(String, @engine.status(first['slug'], portal: true)['claims'].first['machines'].first)
  end

  def test_candidate_bridge_loss_and_colliding_policy_preserve_all_authorities
    first, second = person('first'), person('second')
    a, b = typed(first), typed(second, selector: 'other')
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    identity = {'kind' => 'shared', 'id' => 'joined'}
    candidate = File.join(@tmp, 'collision-candidate')
    FileUtils.cp_r(@package, candidate)
    aliases = [a, b].map { |claim| {'resource' => claim['resources'].first, 'identity' => identity} }
    File.write(File.join(candidate, 'share/dev-workspace/machine-identity-policy.json'), JSON.generate('schema' => 1, 'profileTarget' => nil, 'legacyAliases' => aliases))
    assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(candidate) }.code)
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
    @engine.change_claim(first, a['id'], 'release')
    @engine.change_claim(second, b['id'], 'release')
    legacy_machine
    resolver(label: 'joined')
    policy(aliases: [{'resource' => 'machine:old:host', 'identity' => identity}], profile: identity)
    typed(first)
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    FileUtils.rm_rf(candidate)
    FileUtils.cp_r(@package, candidate)
    File.write(File.join(candidate, 'share/dev-workspace/machine-identity-policy.json'), JSON.generate('schema' => 1, 'profileTarget' => identity, 'legacyAliases' => []))
    assert_equal(5, assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(candidate) }.code)
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
  end

  def test_verify_command_does_not_register_owners_grant_or_write_ledgers
    owner = person('command')
    held = typed(owner)
    host = Object.new.extend(DevWorkspaceHost::WorkspaceCoordinationClaimsHost)
    output = StringIO.new
    host.instance_variable_set(:@out, output)
    current = @engine
    host.define_singleton_method(:coordination_engine) { |_entry| current }
    host.define_singleton_method(:with_coordination_session_admission) { |*_args, &block| block.call }
    paths = %w[claims.json ledger.json].map { |name| File.join(@engine.store.root, name) }
    before = paths.map { |path| File.binread(path) }
    host.coordination_claim(@entry, 'verify-machine', owner['slug'], ['--as-is', '--json', '--id', held['id'], '--repository', 'code', '--selector', 'vps'])
    result = JSON.parse(output.string)
    assert_equal(%w[claimId identifier resource schema sourceRevision], result.keys.sort)
    assert_equal(held['id'], result['claimId'])
    assert_equal(before, paths.map { |path| File.binread(path) })
    assert_equal(4, assert_raises(WorkspaceCoordination::Error) { @engine.verify_machine(person('foreign'), held['id'], 'code', 'vps') }.code)
  end

  def test_typed_profile_requires_exact_source_identity_and_preserves_forward_hold
    owner = person('typed-profile')
    held = typed(owner)
    binding = held['machineBindings'].first
    policy(profile: binding['identity'])
    @engine.instance_variable_set(:@env, @engine.instance_variable_get(:@env).merge('DEV_SESSION_SLUG' => owner['slug'], 'DEV_SESSION_WORKSPACE' => @workspace))
    assert_equal(held, @engine.profile_claim!(held['id'], WorkspaceCoordination::MachineIdentity.resource(binding['identity'])))
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    resolver(label: 'changed')
    assert_equal(3, assert_raises(WorkspaceCoordination::Error) { @engine.profile_claim!(held['id'], WorkspaceCoordination::MachineIdentity.resource(binding['identity'])) }.code)
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
  end

  def test_candidate_admits_active_typed_source_and_preserves_initial_binding_revision
    owner = person('candidate')
    held = typed(owner)
    before = File.binread(File.join(@engine.store.root, 'claims.json'))
    @engine.store.require_claim_capability!(@package)
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
    resolver(label: 'new-label')
    assert_raises(WorkspaceCoordination::Error) { @engine.store.require_claim_capability!(@package) }
    assert_equal(before, File.binread(File.join(@engine.store.root, 'claims.json')))
    assert_equal(held['machineBindings'], @engine.store.claims['claims'][held['id']]['machineBindings'])
  end
end
