# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
  class RegistrationExecStop < StandardError; end

  class RegistrationHost < DevWorkspaceHost::Host
    attr_accessor :responses, :active_override, :before_inventory_write, :fail_marker_verification
    attr_reader :events, :helper_invocations, :execution

    def initialize(package:, responses:, **options)
      super(**options)
      @package = package
      @responses = responses
      @events = []
      @helper_invocations = []
    end

    private

    def package_root
      @package
    end

    def active_codex
      active_override || super
    end

    def run_registration_helper(package, entry)
      @helper_invocations << [package, entry.fetch('name')]
      responses.fetch(entry.fetch('name'))
    end

    def check_codex(codex)
      @events << [:checked, File.realpath(codex)]
    end

    def probe_codex_registration(codex, plan)
      @events << [:probed, File.realpath(codex), plan.digest, plan.argv]
    end

    def quiesce_sessions
      @events << [:quiesced]
      []
    end

    def adopt_codex(codex)
      @events << [:adopted, File.realpath(codex)]
    end

    def restart_codex_consumers
      @events << [:restarted]
      registration_plans.each do |entry, plan|
        write_registration_marker(entry, File.realpath(@system_codex), plan)
      end
    end

    def write_registration_inventory(entry, plan)
      callback = before_inventory_write
      self.before_inventory_write = nil
      callback&.call
      super
    end

    def verify_registration_markers!(plans, codex)
      raise DevWorkspaceHost::Error, 'injected post-restart marker verification failure' if fail_marker_verification

      super
    end

    def wait_for_codex_sockets
      @events << [:sockets_ready]
    end

    def retain_codex_for_generation(_generation, codex)
      @events << [:retained, File.realpath(codex)]
    end

    def profile_generation
      1
    end

    def exec(*argv)
      @execution = argv
      raise RegistrationExecStop
    end
  end

  def test_registration_helper_uses_exact_flags_and_stdin
    Dir.mktmpdir('workspace-agent-team-helper-test') do |directory|
      root, entry, environment = registration_workspace(directory)
      package = File.join(directory, 'package')
      command = File.join(package, 'bin/workspace-portal')
      trace = File.join(directory, 'helper-trace.json')
      FileUtils.mkdir_p(File.dirname(command))
      File.write(command, <<~RUBY)
        #!#{RbConfig.ruby}
        require 'json'
        File.write(ENV.fetch('AGENT_TEAM_HELPER_TRACE'), JSON.generate('argv' => ARGV, 'stdin' => STDIN.read))
        STDOUT.write(ENV.fetch('AGENT_TEAM_HELPER_OUTPUT'))
      RUBY
      File.chmod(0o755, command)
      output = registration_response
      old_trace = ENV['AGENT_TEAM_HELPER_TRACE']
      old_output = ENV['AGENT_TEAM_HELPER_OUTPUT']
      ENV['AGENT_TEAM_HELPER_TRACE'] = trace
      ENV['AGENT_TEAM_HELPER_OUTPUT'] = output
      host = DevWorkspaceHost::Host.new(env: environment, out: StringIO.new, err: StringIO.new)

      host.send(:registration_plan_for, entry, package:)

      invocation = JSON.parse(File.read(trace))
      assert_equal(
        [
          'agent-teams', 'registration', '--package-root', File.realpath(package),
          '--state-root', environment.fetch('DEV_WORKSPACES_STATE'), '--workspace', root
        ],
        invocation.fetch('argv')
      )
      assert_equal("{\"schema\":1}\n", invocation.fetch('stdin'))
    ensure
      ENV['AGENT_TEAM_HELPER_TRACE'] = old_trace
      ENV['AGENT_TEAM_HELPER_OUTPUT'] = old_output
    end
  end

  def test_direct_team_receipt_keeps_member_behavior_and_access
    Dir.mktmpdir('workspace-direct-team-receipt-test') do |directory|
      _root, _entry, environment = registration_workspace(directory)
      host = DevWorkspaceHost::Host.new(env: environment, out: StringIO.new, err: StringIO.new)
      preset = {
        'id' => 'delegated', 'name' => 'Full team', 'description' => 'Direct Codex threads',
        'catalogDigest' => 'a' * 64, 'teamDigest' => 'b' * 64,
        'leadModel' => 'gpt-6-sol', 'leadEffort' => 'high',
        'roles' => %w[lead architect0],
        'members' => [{
          'role' => 'architect', 'address' => 'architect0',
          'behavior' => 'designer', 'access' => 'read_only',
          'model' => 'gpt-6-sol', 'reasoningEffort' => 'xhigh'
        }]
      }

      assert(host.send(:valid_direct_portal_preset?, preset))
      preset['members'].first['access'] = 'workspace_write'
      refute(host.send(:valid_direct_portal_preset?, preset))

      # Current snapshots freeze instructions and access. An old architect may
      # remain read-only while a new architect can write design artifacts.
      preset['leadInstructions'] = "Coordinate the team.\n"
      preset['members'].first['purpose'] = 'design'
      preset['members'].first['instructions'] = 'Write assigned design artifacts.'
      assert(host.send(:valid_direct_portal_preset?, preset))
      preset['members'].first['access'] = 'read_only'
      assert(host.send(:valid_direct_portal_preset?, preset))
      preset['members'].first['purpose'] = 'implementation'
      refute(host.send(:valid_direct_portal_preset?, preset))
      preset['members'].first['purpose'] = 'design'

      path = File.join(directory, 'example.creation.json')
      File.write(path, JSON.generate(
        'schema' => 3, 'slug' => 'example', 'goal_sha256' => 'c' * 64,
        'run_codex' => true, 'state' => 'ready', 'model' => 'gpt-6-sol',
        'effort' => 'high', 'direct_team' => preset
      ))
      File.chmod(0o600, path)
      assert_equal('ready', host.send(:read_private_creation_journal!, path, 'example').fetch('state'))
    end
  end

  def test_registration_plan_rejects_unbounded_or_noncanonical_output
    Dir.mktmpdir('workspace-agent-team-plan-test') do |directory|
      package, entry, host = registration_host(directory)
      invalid = JSON.generate(
        'schema' => 1, 'policy' => 1, 'digest' => 'a' * 64,
        'argv' => ['app-server', '--listen'], 'required_native_child_threads' => 0, 'states' => []
      )
      assert_raises(DevWorkspaceHost::Error) do
        host.send(:decode_registration_plan, invalid, entry, package)
      end
      duplicate = <<~JSON.delete("\n")
        {"schema":1,"schema":1,"policy":1,"digest":"#{'a' * 64}","argv":[],"required_native_child_threads":0,"states":[]}
      JSON
      assert_raises(DevWorkspaceHost::Error) do
        host.send(:decode_registration_plan, duplicate, entry, package)
      end
      assert_raises(DevWorkspaceHost::Error) do
        host.send(:decode_registration_plan, 'x' * (DevWorkspaceHost::REGISTRATION_OUTPUT_MAX_BYTES + 1), entry, package)
      end
    end
  end

  def test_registration_plan_rejects_unsorted_duplicate_or_foreign_state_inventory
    Dir.mktmpdir('workspace-agent-team-inventory-test') do |directory|
      package, entry, host = registration_host(directory)
      later = registered_state(entry.fetch('root'), package)
      earlier = registered_state(entry.fetch('root'), package).merge(
        'slug' => '0-earlier', 'creation_identity' => 'f' * 64, 'root_thread_id' => 'thread-two'
      )
      duplicate = registered_state(entry.fetch('root'), package).merge(
        'creation_identity' => 'e' * 64, 'root_thread_id' => 'thread-three'
      )
      foreign = registered_state(File.join(directory, 'other-workspace'), package)

      [[later, earlier], [later, duplicate], [foreign]].each do |states|
        payload = registration_response(states:)
        assert_raises(DevWorkspaceHost::Error) do
          host.send(:decode_registration_plan, payload, entry, package)
        end
      end
      invalid_slug = registered_state(entry.fetch('root'), package).merge('slug' => '../not-a-session')
      assert_raises(DevWorkspaceHost::Error) do
        host.send(:decode_registration_plan, registration_response(states: [invalid_slug]), entry, package)
      end
      oversized_slug = registered_state(entry.fetch('root'), package).merge('slug' => 'a' * 257)
      assert_raises(DevWorkspaceHost::Error) do
        host.send(:decode_registration_plan, registration_response(states: [oversized_slug]), entry, package)
      end
    end
  end

  def test_registration_helper_terminates_a_subprocess_that_exceeds_the_output_bound
    Dir.mktmpdir('workspace-agent-team-output-bound-test') do |directory|
      root, entry, environment = registration_workspace(directory)
      package = File.join(directory, 'package')
      command = File.join(package, 'bin/workspace-portal')
      pid_path = File.join(directory, 'helper.pid')
      FileUtils.mkdir_p(File.dirname(command))
      File.write(command, <<~RUBY)
        #!#{RbConfig.ruby}
        File.write(ENV.fetch('AGENT_TEAM_HELPER_PID'), Process.pid.to_s)
        STDOUT.write('x' * (4 * 1024 * 1024 + 1))
        STDOUT.flush
        sleep 30
      RUBY
      File.chmod(0o755, command)
      previous = ENV['AGENT_TEAM_HELPER_PID']
      ENV['AGENT_TEAM_HELPER_PID'] = pid_path
      host = DevWorkspaceHost::Host.new(env: environment, out: StringIO.new, err: StringIO.new)

      assert_raises(DevWorkspaceHost::Error) { host.send(:registration_plan_for, entry, package:) }
      pid = Integer(File.read(pid_path), 10)
      assert_raises(Errno::ESRCH) { Process.kill(0, pid) }
    ensure
      ENV['AGENT_TEAM_HELPER_PID'] = previous
    end
  end

  def test_run_codex_writes_private_marker_and_places_all_options_before_app_server
    Dir.mktmpdir('workspace-agent-team-run-test') do |directory|
      package, entry, host = registration_host(
        directory,
        argv: ['-c', 'agents.dw_example.config_file=/nix/store/example-role.toml', '-c', 'agents.max_concurrent_threads_per_session=1']
      )

      assert_raises(RegistrationExecStop) { host.send(:run_codex, [entry.fetch('name')]) }

      execution = host.execution
      app_server = execution.index('app-server')
      assert_equal(File.realpath(host.send(:active_codex)), execution.fetch(0))
      assert_equal(
        ['-c', 'agents.dw_example.config_file=/nix/store/example-role.toml', '-c', 'agents.max_concurrent_threads_per_session=1'],
        execution[1...app_server]
      )
      assert_equal(['app-server', '--listen', "unix://#{host.send(:instance_runtime, entry).fetch(:codex)}"], execution[app_server..])
      marker = host.send(:load_registration_marker, entry)
      assert_equal(0o600, File.stat(host.send(:registration_marker_path, entry)).mode & 0o777)
      assert_equal(
        host.send(:instance_runtime, entry).fetch(:root),
        File.dirname(host.send(:registration_marker_path, entry))
      )
      refute_equal(entry.fetch('root'), File.dirname(host.send(:registration_marker_path, entry)))
      assert_equal(File.realpath(package), marker.dig('launch', 'package_root'))
      assert_equal('a' * 64, marker.dig('launch', 'digest'))
    end
  end

  def test_inventory_refresh_keeps_the_actual_launch_and_updates_only_the_evidence
    Dir.mktmpdir('workspace-agent-team-marker-test') do |directory|
      package_one, entry, host = registration_host(directory)
      plan_one = host.send(:registration_plan_for, entry, package: package_one)
      codex = File.realpath(host.send(:active_codex))
      host.send(:write_registration_marker, entry, codex, plan_one)
      package_two = File.join(directory, 'package-two')
      FileUtils.mkdir_p(package_two)
      state = registered_state(entry.fetch('root'), package_two)
      host.responses = { entry.fetch('name') => registration_response(states: [state]) }
      plan_two = host.send(:registration_plan_for, entry, package: package_two)

      host.send(:refresh_registration_inventory, entry, plan_two)

      marker = host.send(:load_registration_marker, entry)
      inventory = host.send(:load_registration_inventory, entry)
      assert_equal(File.realpath(package_one), marker.dig('launch', 'package_root'))
      assert_equal(File.realpath(package_two), inventory.fetch('package_root'))
      assert_equal([state], inventory.fetch('states'))
    end
  end

  def test_inventory_refresh_never_overwrites_a_newer_launch_marker
    Dir.mktmpdir('workspace-agent-team-marker-race-test') do |directory|
      package, entry, host = registration_host(directory)
      codex = File.realpath(host.send(:active_codex))
      original = host.send(:registration_plan_for, entry, package:)
      host.send(:write_registration_marker, entry, codex, original)
      refreshed = host.send(
        :decode_registration_plan, registration_response(digest: 'b' * 64), entry, package
      )
      newer = host.send(
        :decode_registration_plan, registration_response(digest: 'c' * 64), entry, package
      )
      host.before_inventory_write = proc do
        host.send(:write_registration_marker, entry, codex, newer)
      end

      host.send(:refresh_registration_inventory, entry, refreshed)

      marker = host.send(:load_registration_marker, entry)
      assert_equal('c' * 64, marker.dig('launch', 'digest'))
    end
  end

  def test_inventory_only_reconciliation_updates_evidence_without_a_restart
    Dir.mktmpdir('workspace-agent-team-inventory-reconcile-test') do |directory|
      package, entry, host = registration_host(directory)
      codex = File.realpath(host.send(:active_codex))
      plan = host.send(:registration_plan_for, entry, package:)
      host.send(:write_registration_marker, entry, codex, plan)
      state = registered_state(entry.fetch('root'), package)
      host.responses = { entry.fetch('name') => registration_response(states: [state]) }

      assert(host.send(:reconcile_codex_update, defer_busy: false))

      refute_includes(host.events, [:restarted])
      assert_equal([state], host.send(:load_registration_inventory, entry).fetch('states'))
    end
  end

  def test_reconciliation_uses_semantic_digest_for_restart_and_keeps_strict_pending_evidence
    Dir.mktmpdir('workspace-agent-team-reconcile-test') do |directory|
      package, entry, host = registration_host(directory)
      codex = File.realpath(host.send(:active_codex))
      plan = host.send(:registration_plan_for, entry, package:)
      host.send(:write_registration_marker, entry, codex, plan)

      assert(host.send(:reconcile_codex_update, defer_busy: false))
      refute_includes(host.events, [:restarted])

      host.responses = { entry.fetch('name') => registration_response(digest: 'b' * 64) }
      assert(host.send(:reconcile_codex_update, defer_busy: false))
      assert_includes(host.events, [:checked, codex])
      assert(host.events.any? { |event| event.first == :probed && event[2] == 'b' * 64 })
      assert_includes(host.events, [:quiesced])
      assert_includes(host.events, [:restarted])
      assert_nil(host.send(:pending_codex_update))
      marker = host.send(:load_registration_marker, entry)
      assert_equal('b' * 64, marker.dig('launch', 'digest'))

      plans = host.send(:registration_plans, package:)
      host.send(:write_pending_codex_update, package, codex, plans)
      pending = host.send(:pending_codex_update)
      assert_equal(1, pending.fetch('schema'))
      assert_equal(['example-workspace'], pending.fetch('workspaces').map { |item| item.fetch('name') })
      File.write(host.send(:pending_codex_file), "#{codex}\n")
      File.chmod(0o600, host.send(:pending_codex_file))
      error = assert_raises(DevWorkspaceHost::Error) { host.send(:pending_codex_update) }
      assert_includes(error.message, 'forward migration is required')
    end
  end

  def test_pending_evidence_is_recomputed_after_digest_drift_and_marker_verification_failure
    Dir.mktmpdir('workspace-agent-team-pending-recompute-test') do |directory|
      package, entry, host = registration_host(directory)
      codex = File.realpath(host.send(:active_codex))
      original = host.send(:registration_plan_for, entry, package:)
      host.send(:write_registration_marker, entry, codex, original)
      host.send(:write_pending_codex_update, package, codex, [[entry, original]])
      host.responses = { entry.fetch('name') => registration_response(digest: 'b' * 64) }
      host.fail_marker_verification = true

      assert_raises(DevWorkspaceHost::Error) do
        host.send(:reconcile_codex_update, defer_busy: false)
      end

      pending = host.send(:pending_codex_update)
      assert_equal('b' * 64, pending.fetch('workspaces').fetch(0).fetch('registration_digest'))
      assert_includes(host.events, [:restarted])
    end
  end

  def test_reconciliation_restarts_for_a_different_binary_even_without_registered_workspaces
    Dir.mktmpdir('workspace-agent-team-empty-binary-test') do |directory|
      config = File.join(directory, 'config', 'registry.json')
      package = File.join(directory, 'package')
      FileUtils.mkdir_p(package)
      old_codex = make_codex(directory, 'codex-old')
      host = RegistrationHost.new(
        package:, responses: {}, env: host_environment(directory, config:),
        out: StringIO.new, err: StringIO.new
      )
      host.active_override = old_codex

      assert(host.send(:reconcile_codex_update, defer_busy: false))

      assert_includes(host.events, [:checked, File.realpath(host.instance_variable_get(:@system_codex))])
      assert_includes(host.events, [:restarted])
    end
  end

  def test_reconciliation_restarts_for_a_different_binary_with_an_unchanged_digest
    Dir.mktmpdir('workspace-agent-team-binary-test') do |directory|
      package, entry, host = registration_host(directory)
      old_codex = make_codex(directory, 'codex-old')
      plan = host.send(:registration_plan_for, entry, package:)
      host.send(:write_registration_marker, entry, File.realpath(old_codex), plan)
      host.active_override = old_codex

      assert(host.send(:reconcile_codex_update, defer_busy: false))

      assert_includes(host.events, [:restarted])
      assert_equal('a' * 64, host.send(:load_registration_marker, entry).dig('launch', 'digest'))
      assert_equal(File.realpath(host.instance_variable_get(:@system_codex)), host.send(:load_registration_marker, entry).dig('launch', 'codex_path'))
    end
  end

  def test_registration_marker_symlink_is_a_hard_error_while_malformed_contents_require_restart
    Dir.mktmpdir('workspace-agent-team-marker-safety-test') do |directory|
      package, entry, host = registration_host(directory)
      path = host.send(:registration_marker_path, entry)
      FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
      File.symlink(File.join(directory, 'elsewhere'), path)
      assert_raises(DevWorkspaceHost::Error) { host.send(:load_registration_marker, entry) }
      File.unlink(path)
      File.write(path, "{\n")
      File.chmod(0o600, path)
      assert_equal(:malformed, host.send(:load_registration_marker, entry))
      assert(host.send(:registration_restart_required?, host.send(:registration_plans, package:), File.realpath(host.send(:active_codex))))
    end
  end

  def test_portal_managed_creation_scan_ignores_terminal_evidence_and_blocks_open_receipts
    Dir.mktmpdir('workspace-agent-team-portal-creation-test') do |directory|
      _package, entry, host = registration_host(directory)
      workspace = entry.fetch('root')
      slug = '2026-09-22-managed'
      receipt_id = 'b' * 64
      creations = File.join(
        host.instance_variable_get(:@state), 'portal',
        "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}", 'creations'
      )
      FileUtils.mkdir_p(creations, mode: 0o700)
      File.chmod(0o700, creations)
      evidence = File.join(creations, "#{slug}.#{receipt_id}.complete.json")
      File.write(evidence, "terminal companion\n")
      File.chmod(0o600, evidence)
      receipt_path = File.join(creations, "#{slug}.json")
      receipt = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'ready')
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)

      assert_equal([], host.send(:unfinished_portal_managed_creations, entry))

      receipt['state'] = 'running'
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)
      assert_equal(
        ["#{entry.fetch('name')}/#{slug} (portal managed creation)"],
        host.send(:unfinished_portal_managed_creations, entry)
      )

      receipt['state'] = 'conflict'
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)
      assert_equal([], host.send(:unfinished_portal_managed_creations, entry))

      plan_request = lambda do
        {
          'kind' => 'plan', 'slug' => slug, 'source' => 'source',
          'sourceThreadId' => 'source-thread', 'sourceIdentity' => 'e' * 64,
          'planTurnId' => 'plan-turn', 'planSha256' => 'f' * 64,
          'team' => 'solo', 'catalogDigest' => 'd' * 64
        }
      end
      receipt['state'] = 'cancelled'
      receipt['validated'] = false
      receipt['request'] = plan_request.call
      File.unlink(evidence)
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)
      assert_equal([], host.send(:unfinished_portal_managed_creations, entry))

      File.write(evidence, "terminal companion\n")
      File.chmod(0o600, evidence)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }
      File.unlink(evidence)

      receipt['request'] = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'cancelled').fetch('request')
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }

      receipt['request'] = plan_request.call
      receipt['validated'] = true
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }

      receipt['validated'] = false
      FileUtils.mkdir_p(File.join(workspace, 'work', slug), mode: 0o700)
      File.write(receipt_path, JSON.generate(receipt))
      File.chmod(0o600, receipt_path)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }
      FileUtils.remove_dir(File.join(workspace, 'work', slug))

      File.write(File.join(creations, 'not a receipt.json'), "{}")
      File.chmod(0o600, File.join(creations, 'not a receipt.json'))
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }
    end
  end

  def test_portal_creation_scan_branches_schema_and_rejects_cross_kind_managed_requests
    Dir.mktmpdir('workspace-agent-team-portal-creation-schema-test') do |directory|
      _package, entry, host = registration_host(directory)
      workspace = entry.fetch('root')
      slug = '2026-09-22-managed'
      receipt_id = 'b' * 64
      creations = File.join(
        host.instance_variable_get(:@state), 'portal',
        "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}", 'creations'
      )
      FileUtils.mkdir_p(creations, mode: 0o700)
      File.chmod(0o700, creations)
      receipt_path = File.join(creations, "#{slug}.json")
      write = lambda do |receipt|
        File.write(receipt_path, JSON.generate(receipt))
        File.chmod(0o600, receipt_path)
      end

      [nil, 3, 1.0].each do |schema|
        receipt = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'ready')
        schema.nil? ? receipt.delete('schema') : receipt['schema'] = schema
        write.call(receipt)
        assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }
      end

      File.write(receipt_path, '{"schema":2,"schema":1}')
      File.chmod(0o600, receipt_path)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }

      receipt = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'ready')
      receipt.fetch('request')['source'] = 'source'
      write.call(receipt)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }

      receipt = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'ready')
      receipt['request'] = {
        'kind' => 'plan', 'slug' => slug, 'source' => 'source', 'sourceThreadId' => 'source-thread',
        'sourceIdentity' => 'e' * 64, 'planTurnId' => 'plan-turn', 'planSha256' => 'f' * 64,
        'team' => 'solo', 'catalogDigest' => 'd' * 64, 'goal' => 'not a plan field'
      }
      write.call(receipt)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }
    end
  end

  def test_portal_creation_scan_ignores_schema_1_legacy_receipts
    Dir.mktmpdir('workspace-agent-team-portal-legacy-creation-test') do |directory|
      _package, entry, host = registration_host(directory)
      workspace = entry.fetch('root')
      slug = '2026-09-22-legacy'
      receipt_id = 'b' * 64
      creations = File.join(
        host.instance_variable_get(:@state), 'portal',
        "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}", 'creations'
      )
      FileUtils.mkdir_p(creations, mode: 0o700)
      File.chmod(0o700, creations)
      receipt_path = File.join(creations, "#{slug}.json")
      write = lambda do |receipt|
        File.write(receipt_path, JSON.generate(receipt))
        File.chmod(0o600, receipt_path)
      end

      legacy = legacy_portal_creation_receipt(workspace, slug, receipt_id)
      legacy['legacy_only'] = { 'state' => 'unvalidated' }
      File.write(receipt_path, JSON.generate(legacy))
      File.chmod(0o644, receipt_path)
      assert_equal([], host.send(:unfinished_portal_managed_creations, entry))

      legacy['state'] = 'running'
      write.call(legacy)
      assert_equal([], host.send(:unfinished_portal_managed_creations, entry))
    end
  end

  def test_portal_managed_creation_scan_accepts_independent_overrides_at_runtime_text_bounds
    Dir.mktmpdir('workspace-agent-team-portal-creation-bounds-test') do |directory|
      _package, entry, host = registration_host(directory)
      workspace = entry.fetch('root')
      slug = '2026-09-22-managed'
      receipt_id = 'b' * 64
      creations = File.join(
        host.instance_variable_get(:@state), 'portal',
        "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}", 'creations'
      )
      FileUtils.mkdir_p(creations, mode: 0o700)
      File.chmod(0o700, creations)
      receipt_path = File.join(creations, "#{slug}.json")
      write = lambda do |receipt|
        File.write(receipt_path, JSON.generate(receipt))
        File.chmod(0o600, receipt_path)
      end

      receipt = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'ready')
      receipt.fetch('request')['model'] = 'm' * DevWorkspaceHost::AGENT_TEAM_PERSISTED_SCALAR_MAX_BYTES
      receipt.fetch('request')['effort'] = 'e' * DevWorkspaceHost::AGENT_TEAM_PERSISTED_SCALAR_MAX_BYTES
      receipt.fetch('request')['goal'] = 'g' * 20_000
      write.call(receipt)
      assert_equal([], host.send(:unfinished_portal_managed_creations, entry))

      receipt.fetch('request')['model'] << 'm'
      write.call(receipt)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }

      receipt = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'ready')
      receipt.fetch('request')['goal'] = 'g' * 20_000
      receipt.fetch('request')['goal'] << 'g'
      write.call(receipt)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }

      receipt = managed_portal_creation_receipt(workspace, slug, receipt_id, state: 'ready')
      receipt['request'] = {
        'kind' => 'plan', 'slug' => slug, 'source' => 'source', 'sourceThreadId' => 'source-thread',
        'sourceIdentity' => 'e' * 64, 'planTurnId' => 'plan-turn', 'planSha256' => 'f' * 64,
        'team' => 'solo', 'catalogDigest' => 'd' * 64, 'effort' => 'high', 'planText' => 'p' * 20_000
      }
      write.call(receipt)
      assert_equal([], host.send(:unfinished_portal_managed_creations, entry))

      receipt.fetch('request')['planText'] << 'p'
      write.call(receipt)
      assert_raises(DevWorkspaceHost::Error) { host.send(:unfinished_portal_managed_creations, entry) }
    end
  end

  def test_agent_team_registration_contract_matches_the_published_runtime_contract
    contract = JSON.parse(
      File.read(File.expand_path('../../portal/internal/session/runtime-contract.json', __dir__))
    ).fetch('agentTeamRegistration')

    assert_equal(contract, DevWorkspaceHost::AGENT_TEAM_REGISTRATION)
    assert_equal(4 * 1024 * 1024, DevWorkspaceHost::REGISTRATION_OUTPUT_MAX_BYTES)
    assert_equal(contract.fetch('maxPersistedScalarBytes'), DevWorkspaceHost::AGENT_TEAM_PERSISTED_SCALAR_MAX_BYTES)
  end

  private

  def registration_workspace(directory)
    root = make_workspace(directory, 'workspace')
    config = File.join(directory, 'config', 'registry.json')
    entry = DevWorkspaceHost::Registry.new(config).register(
      name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
      aliases: [], replace: false
    )
    [root, entry, host_environment(directory, config:)]
  end

  def registration_host(directory, argv: ['-c', 'agents.dw_example.config_file=/nix/store/example-role.toml'])
    _root, entry, environment = registration_workspace(directory)
    package = File.join(directory, 'package')
    FileUtils.mkdir_p(package)
    host = RegistrationHost.new(
      package:, responses: { entry.fetch('name') => registration_response(argv:) }, env: environment,
      out: StringIO.new, err: StringIO.new
    )
    [package, entry, host]
  end

  def registration_response(digest: 'a' * 64, argv: ['-c', 'agents.dw_example.config_file=/nix/store/example-role.toml'], states: [])
    JSON.generate(
      'schema' => 1, 'policy' => 1, 'digest' => digest, 'argv' => argv,
      'required_native_child_threads' => argv.empty? ? 0 : 1, 'states' => states
    )
  end

  def registered_state(workspace, package)
    {
      'state_revision' => 1, 'selection_revision' => 1, 'workspace' => workspace,
      'slug' => '2026-09-22-agent-team', 'removal_epoch' => 'c' * 64,
      'creation_identity' => 'd' * 64, 'root_thread_id' => 'thread-one',
      'catalog' => {
        'package_path' => File.realpath(package), 'catalog_path' => 'share/dev-workspace/agent-teams.json',
        'catalog_digest' => 'e' * 64, 'schema_version' => 3, 'metadata_schema' => 1,
        'generator' => { 'identity' => 'dev-workspace.agent-teams', 'version' => 1, 'canonicalization' => 'nix-json-v1' },
        'native_adapter' => { 'identity' => 'codex-native-roles', 'version' => 1 },
        'capacity' => { 'required_native_child_threads' => 1 }
      }
    }
  end

  def managed_portal_creation_receipt(workspace, slug, receipt_id, state:)
    binding_digest = 'a' * 64
    {
      'schema' => 2, 'workspace' => workspace, 'receiptId' => receipt_id,
      'deletionHistorySha256' => 'c' * 64, 'attempt' => 1, 'state' => state,
      'phase' => 'Creating', 'startedAt' => '2026-09-22T12:00:00Z',
      'updatedAt' => '2026-09-22T12:00:00Z', 'validated' => true,
      'model' => 'model-lead', 'effort' => 'high',
      'agentTeamBinding' => "v1.eyJ4IjoieSJ9.#{binding_digest}",
      'agentTeamBindingDigest' => binding_digest,
      'request' => {
        'kind' => 'new', 'slug' => slug, 'goal' => 'Create managed session',
        'team' => 'solo', 'catalogDigest' => 'd' * 64
      }
    }
  end

  def legacy_portal_creation_receipt(workspace, slug, receipt_id)
    {
      'schema' => 1, 'workspace' => workspace, 'receiptId' => receipt_id,
      'deletionHistorySha256' => 'c' * 64, 'attempt' => 1, 'state' => 'ready',
      'phase' => 'Creating', 'startedAt' => '2026-09-22T12:00:00Z',
      'updatedAt' => '2026-09-22T12:00:00Z', 'validated' => true,
      'request' => { 'kind' => 'new', 'slug' => slug, 'goal' => 'Create unmanaged session' }
    }
  end
end
