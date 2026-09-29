# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
  module RecoveryBehavior
    attr_accessor :recovery_journal, :fail_recovery_preflight, :fail_recovery_executor,
                  :change_profile_before_lock

    private

    def codex_version(_command)
      '0.155.0'
    end

    def with_transition_lock(mode = File::LOCK_EX, &block)
      if change_profile_before_lock
        self.change_profile_before_lock = false
        File.unlink(@profile)
        File.symlink(candidate, @profile)
      end
      super(mode, &block)
    end

    def capture_env!(environment, *argv)
      return super unless argv.include?('require-archived')

      events << [:archive_preflight, environment, argv]
      raise DevWorkspaceHost::Error, 'injected archive proof failure' if fail_recovery_preflight

      ''
    end

    def system_env_interactive!(environment, command, *arguments)
      events << [:archive_executor, environment, command, arguments]
      raise DevWorkspaceHost::Error, 'injected lifecycle interruption' if fail_recovery_executor

      File.unlink(recovery_journal)
    end
  end

  def test_recover_archive_uses_selected_executor_and_only_substitutes_protocol_helper
    with_recovery_host do |host, paths|
      creation_before = File.binread(paths.fetch(:creation))
      assert_equal(0, recover_archive(host, paths))
      preflights = host.events.select { |event| event.first == :archive_preflight }
      assert_equal(%w[thread team], preflights.map { |event| event.fetch(2).fetch(1) })
      assert(preflights.all? { |event| event.fetch(1).fetch('DEV_WORKSPACE_CODEX_HOME') == paths.fetch(:codex_home) })
      execution = host.events.find { |event| event.first == :archive_executor }
      refute_nil(execution)
      assert_equal(File.join(paths.fetch(:predecessor), 'libexec/workspace-portal/dev-session'), execution.fetch(2))
      entry = host.send(:registry).find('example-workspace')
      _, expected_command, expected_arguments = host.send(
        :dev_session_invocation, entry, ['archive', paths.fetch(:slug), '--as-is'],
        transition_lock: false, package: paths.fetch(:predecessor)
      )
      assert_equal(expected_command, execution.fetch(2))
      index = expected_arguments.index('--portal-command')
      expected_arguments[index + 1] = File.join(paths.fetch(:candidate), 'bin/workspace-portal')
      assert_equal(expected_arguments, execution.fetch(3))
      assert_equal(paths.fetch(:predecessor), File.realpath(host.instance_variable_get(:@profile)))
      refute(File.exist?(paths.fetch(:journal)))
      assert_equal(creation_before, File.binread(paths.fetch(:creation)))
      refute(host.events.any? { |event| %i[profile_set configured consumers_restarted sessions_quiesced].include?(event.first) })
    end
  end

  def test_recover_archive_accepts_absent_legacy_creation_record
    with_recovery_host(creation_record: false) do |host, paths|
      refute(File.exist?(paths.fetch(:creation)))
      assert_equal(0, recover_archive(host, paths))
      assert(host.events.any? { |event| event.first == :archive_executor })
    end
  end

  def test_recover_archive_accepts_supported_ready_creation_variants
    %w[managed direct legacy-null-goal].each do |variant|
      with_recovery_host do |host, paths|
        record = JSON.parse(File.read(paths.fetch(:creation)))
        case variant
        when 'managed', 'direct'
          %w[preserve_tracking tracking_origin tracking_plan_sha256 tracking_state_sha256].each do |key|
            record.delete(key)
          end
          record['model'] = 'gpt-6-sol'
          record['effort'] = 'high'
          if variant == 'managed'
            record['schema'] = 2
            record['agent_team_binding'] = "v1.payload.#{'a' * 64}"
            record['agent_team_binding_digest'] = 'a' * 64
          else
            record['schema'] = 3
            record['direct_team'] = {
              'id' => 'delegated', 'name' => 'Full team', 'description' => 'Direct threads',
              'catalogDigest' => 'a' * 64, 'teamDigest' => 'b' * 64,
              'leadModel' => 'gpt-6-sol', 'leadEffort' => 'high',
              'leadInstructions' => "Coordinate the team.\n", 'roles' => %w[lead architect0],
              'members' => [{
                'role' => 'architect', 'address' => 'architect0', 'behavior' => 'designer',
                'purpose' => 'design', 'access' => 'read_only',
                'instructions' => 'Write assigned design artifacts.',
                'model' => 'gpt-6-sol', 'reasoningEffort' => 'xhigh'
              }]
            }
          end
        when 'legacy-null-goal'
          record['goal_sha256'] = nil
          manifest = YAML.safe_load(File.read(paths.fetch(:manifest)))
          manifest.fetch('creation').delete('goal_sha256')
          File.write(paths.fetch(:manifest), YAML.dump(manifest))
        end
        File.write(paths.fetch(:creation), JSON.generate(record))
        creation_before = File.binread(paths.fetch(:creation))
        assert_equal(0, recover_archive(host, paths), variant)
        assert(host.events.any? { |event| event.first == :archive_executor }, variant)
        assert_equal(creation_before, File.binread(paths.fetch(:creation)), variant)
      end
    end
  end

  def test_recover_archive_refuses_unfinished_or_invalid_creation_before_proof
    %w[creating malformed wrong-slug unsafe-mode goal-mismatch invalid-tracking unknown-schema
       unknown-state oversized symlink nonregular explicit-null-goal].each do |failure|
      with_recovery_host do |host, paths|
        creation = paths.fetch(:creation)
        case failure
        when 'creating'
          record = JSON.parse(File.read(creation))
          record['state'] = 'creating'
          record['tmux_identity'] = 'f' * 64
          File.write(creation, JSON.generate(record))
        when 'malformed'
          File.write(creation, '{')
        when 'wrong-slug', 'invalid-tracking', 'unknown-schema', 'unknown-state'
          record = JSON.parse(File.read(creation))
          case failure
          when 'wrong-slug'
            record['slug'] = 'wrong'
          when 'invalid-tracking'
            record['tracking_plan_sha256'] = 'wrong'
          when 'unknown-schema'
            record['schema'] = 99
          when 'unknown-state'
            record['state'] = 'unknown'
          end
          File.write(creation, JSON.generate(record))
        when 'unsafe-mode'
          File.chmod(0o644, creation)
        when 'goal-mismatch'
          manifest = YAML.safe_load(File.read(paths.fetch(:manifest)))
          manifest.fetch('creation')['goal_sha256'] = 'f' * 64
          File.write(paths.fetch(:manifest), YAML.dump(manifest))
        when 'oversized'
          File.write(creation, JSON.generate(JSON.parse(File.read(creation)).merge('padding' => 'x' * 65_536)))
        when 'symlink'
          File.rename(creation, "#{creation}.target")
          File.symlink("#{creation}.target", creation)
        when 'nonregular'
          File.unlink(creation)
          Dir.mkdir(creation)
        when 'explicit-null-goal'
          record = JSON.parse(File.read(creation))
          record['goal_sha256'] = nil
          File.write(creation, JSON.generate(record))
          manifest = YAML.safe_load(File.read(paths.fetch(:manifest)))
          manifest.fetch('creation')['goal_sha256'] = nil
          File.write(paths.fetch(:manifest), YAML.dump(manifest))
        end
        assert_recovery_refused_before_proof(host, paths, failure)
      end
    end
  end

  def test_recover_archive_refuses_competing_operations_with_ready_creation
    %w[start fork agent-teams-migration removal revive].each do |name|
      with_recovery_host do |host, paths|
        conflict = File.join(File.dirname(paths.fetch(:journal)), "#{paths.fetch(:slug)}.#{name}.json")
        File.write(conflict, '{}')
        assert_recovery_refused_before_proof(host, paths, name)
      end
    end
  end

  def test_recover_archive_refuses_wrong_source_phase_identity_and_missing_journal
    %w[source phase workspace root missing conflict contract generation].each do |failure|
      with_recovery_host do |host, paths|
        case failure
        when 'source'
          host.executing_package = paths.fetch(:predecessor)
        when 'phase', 'workspace'
          journal = JSON.parse(File.read(paths.fetch(:journal)))
          journal[failure == 'phase' ? 'phase' : 'workspace'] = 'wrong'
          File.write(paths.fetch(:journal), JSON.generate(journal))
        when 'root'
          manifest = YAML.safe_load(File.read(paths.fetch(:manifest)))
          manifest.fetch('codex')['thread_id'] = 'other'
          File.write(paths.fetch(:manifest), YAML.dump(manifest))
        when 'missing'
          File.unlink(paths.fetch(:journal))
        when 'conflict'
          File.write(File.join(File.dirname(paths.fetch(:journal)), "#{paths.fetch(:slug)}.removal.json"), "{}")
        when 'contract'
          contract = File.join(paths.fetch(:predecessor), 'share/workspace-portal/runtime-contract.json')
          data = JSON.parse(File.read(contract))
          data.delete('devSessionFlags')
          File.write(contract, JSON.generate(data))
        when 'generation'
          host.change_profile_before_lock = true
        end
        assert_equal(1, recover_archive(host, paths), failure)
        refute(host.events.any? { |event| event.first == :archive_executor }, failure)
        assert_equal(paths.fetch(:predecessor), File.realpath(host.instance_variable_get(:@profile))) unless failure == 'generation'
      end
    end
  end

  def test_recover_archive_keeps_journal_on_failed_proof_or_executor_and_retries
    with_recovery_host do |host, paths|
      host.fail_recovery_preflight = true
      assert_equal(1, recover_archive(host, paths))
      assert(File.file?(paths.fetch(:journal)))
      refute(host.events.any? { |event| event.first == :archive_executor })
      host.fail_recovery_preflight = false
      host.fail_recovery_executor = true
      assert_equal(1, recover_archive(host, paths))
      assert(File.file?(paths.fetch(:journal)))
      host.fail_recovery_executor = false
      assert_equal(0, recover_archive(host, paths))
      refute(File.exist?(paths.fetch(:journal)))
      assert_equal(1, recover_archive(host, paths))
    end
  end

  def test_recover_archive_preserves_existing_abandoned_mode_without_force
    with_recovery_host do |host, paths|
      journal = JSON.parse(File.read(paths.fetch(:journal)))
      journal['mode'] = 'abandoned'
      File.write(paths.fetch(:journal), JSON.generate(journal))
      assert_equal(0, recover_archive(host, paths))
      arguments = host.events.find { |event| event.first == :archive_executor }.fetch(3)
      assert_equal(%w[archive 2026-09-29-recover-example --as-is --abandoned],
                   arguments[(arguments.index('--') + 1)..])
      refute_includes(arguments, '--force')
      refute_includes(arguments, '--portal-authorized')
    end
  end

  def test_normal_switch_still_refuses_journal_then_succeeds_after_recovery
    with_recovery_host do |host, paths|
      host.executing_package = nil
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.executing_package = paths.fetch(:candidate)
      assert_equal(0, recover_archive(host, paths))
      host.executing_package = nil
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(paths.fetch(:candidate), File.realpath(host.instance_variable_get(:@profile)))
    end
  end

  private

  def assert_recovery_refused_before_proof(host, paths, failure)
    journal_before = File.binread(paths.fetch(:journal))
    assert_equal(1, recover_archive(host, paths), failure)
    refute(host.events.any? { |event| %i[archive_preflight archive_executor].include?(event.first) }, failure)
    assert_equal(journal_before, File.binread(paths.fetch(:journal)), failure)
    assert_equal(paths.fetch(:predecessor), File.realpath(host.instance_variable_get(:@profile)), failure)
  end

  def recover_archive(host, paths)
    host.run('workspace-host', [
      'recover-archive', '--source', paths.fetch(:source),
      '--workspace', 'example-workspace', '--session', paths.fetch(:slug)
    ])
  end

  def with_recovery_host(creation_record: true)
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      predecessor = File.realpath(host.instance_variable_get(:@profile))
      candidate = make_package(paths.fetch(:root), 'package-two')
      [predecessor, candidate].each do |package|
        File.write(File.join(package, 'share/workspace-portal/runtime-contract.json'),
                   JSON.generate(DevWorkspaceHost::RUNTIME_CONTRACT))
        helper = File.join(package, 'bin/workspace-portal')
        File.write(helper, "#!/bin/sh\nexit 0\n")
        File.chmod(0o755, helper)
      end
      executor = File.join(predecessor, 'libexec/workspace-portal/dev-session')
      FileUtils.mkdir_p(File.dirname(executor))
      File.write(executor, "#!/bin/sh\nexit 0\n")
      File.chmod(0o755, executor)
      codex_home = File.join(paths.fetch(:root), '.codex')
      FileUtils.mkdir_p(codex_home)
      host.candidate = candidate
      host.executing_package = candidate
      host.extend(RecoveryBehavior)
      host.events.clear
      entry = host.send(:registry).find('example-workspace')
      slug = '2026-09-29-recover-example'
      root_thread = '11111111-1111-7111-8111-111111111111'
      locks = File.join(entry.fetch('root'), 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, "#{slug}.archive.json")
      File.write(journal, JSON.generate(
        'schema' => 2, 'slug' => slug, 'workspace' => entry.fetch('root'),
        'operation_id' => 'a' * 64, 'phase' => 'tracking_committed',
        'mode' => 'complete', 'proven_heads' => {}, 'finalized_at' => '2026-09-29T10:00:00Z',
        'retained_thread_id' => root_thread, 'target_tracking_sha256' => 'b' * 64
      ))
      File.chmod(0o600, journal)
      archive = File.join(entry.fetch('root'), 'archive', slug)
      FileUtils.mkdir_p(archive)
      manifest = File.join(archive, 'portal.yml')
      File.write(manifest, YAML.dump(
        'slug' => slug, 'codex' => { 'thread_id' => root_thread,
                                    'socket_path' => host.send(:instance_runtime, entry).fetch(:codex) },
        'creation' => { 'state' => 'ready', 'goal_sha256' => 'c' * 64 }
      ))
      creation = File.join(locks, "#{slug}.creation.json")
      if creation_record
        File.write(creation, JSON.generate(
          'schema' => 1, 'slug' => slug, 'goal_sha256' => 'c' * 64,
          'run_codex' => true, 'state' => 'ready', 'preserve_tracking' => true,
          'tracking_origin' => 'retained', 'tracking_plan_sha256' => 'd' * 64,
          'tracking_state_sha256' => 'e' * 64
        ))
        File.chmod(0o600, creation)
      end
      host.recovery_journal = journal
      yield host, paths.merge(predecessor:, candidate:, codex_home:, journal:, manifest:, creation:, slug:)
    end
  end
end
