# frozen_string_literal: true

require_relative 'suspension_test'

class WorkspaceHostTest < Minitest::Test
  def test_busy_switch_preserves_consumers_and_launch_marker_across_catalog_changes
    [nil, 'b' * 64].each do |digest|
      with_transition_host do |host, paths|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        entry = host.send(:registry).entries.fetch(0)
        marker = host.send(:load_registration_marker, entry)
        host.events.clear
        host.busy = ['lead and member are active']
        host.registration_digest = digest
        host.candidate = make_package(paths.fetch(:root), 'package-two')

        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        assert_equal(host.candidate, File.realpath(host.instance_variable_get(:@profile)))
        refute(host.events.any? { |event| %i[sessions_quiesced sessions_restored consumers_restarted].include?(event.first) })
        assert_includes(host.events, [:configured])
        assert_equal(marker, host.send(:load_registration_marker, entry))
        assert_equal(host.candidate, host.send(:load_registration_inventory, entry).fetch('package_root'))
        assert_nil(host.send(:pending_codex_update))
      end
    end
  end

  def test_busy_switch_refuses_missing_malformed_or_incompatible_launch_evidence
    %i[missing malformed argv capacity legacy].each do |mismatch|
      with_transition_host do |host, paths|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        entry = host.send(:registry).entries.fetch(0)
        marker_path = host.send(:registration_marker_path, entry)
        case mismatch
        when :missing then File.unlink(marker_path)
        when :malformed then File.write(marker_path, '{}')
        when :argv then host.registration_argv = []
        when :capacity then host.registration_capacity = 2
        when :legacy
          contract = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
          data = JSON.parse(File.read(contract))
          data.delete('livePackageSwitchPolicy')
          File.write(contract, JSON.generate(data))
        end
        predecessor = host.candidate
        host.candidate = make_package(paths.fetch(:root), 'package-two')
        host.busy = ['active member']
        host.events.clear

        assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]), mismatch.to_s)
        assert_equal(predecessor, File.realpath(host.instance_variable_get(:@profile)))
        refute(host.events.any? { |event| event.first == :profile_set })
        assert_equal(host.candidate, host.send(:pending_codex_update).fetch('package_root'))
        host.busy = []
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        assert_includes(host.events, [:consumers_restarted])
      end
    end
  end

  def test_switch_checks_and_adopts_the_candidates_binary
    with_transition_host do |host, paths|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.instance_variable_get(:@env).delete('DEV_WORKSPACES_SYSTEM_CODEX')
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      codex = make_codex(paths.fetch(:root), 'codex-candidate')
      bundled = File.join(host.candidate, 'libexec/codex/bin/codex')
      FileUtils.mkdir_p(File.dirname(bundled))
      File.symlink(codex, bundled)
      host.events.clear
      host.busy = ['active lead']

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(codex), host.send(:pending_codex_update).fetch('codex_path'))
      assert(host.events.any? { |event| event == [:registration_probed, File.realpath(codex), 'a' * 64] })
      host.busy = []
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(codex), File.realpath(host.send(:active_codex)))
    end
  end

  def test_failed_live_activation_retains_forward_target_and_retries_while_busy
    with_transition_host do |host, paths|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.busy = ['active lead']
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_activation = true
      host.events.clear

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(host.candidate, File.realpath(host.instance_variable_get(:@profile)))
      assert_equal(host.candidate, host.send(:pending_codex_update).fetch('package_root'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(host.events.any? { |event| %i[sessions_quiesced sessions_restored consumers_restarted].include?(event.first) })
      assert_nil(host.send(:pending_codex_update))
    end
  end
end
