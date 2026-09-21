# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
  def test_suspend_quiesces_sessions_before_disabling_the_user_services
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.events.clear

      assert_equal(0, host.run('workspace-host', ['suspend']))

      assert_equal(:sessions_quiesced, host.events.fetch(0).first)
      disable = host.events.find do |event|
        event[0, 4] == [:command, 'systemctl', '--user', 'disable']
      end
      refute_nil(disable)
      assert_includes(disable, '--now')
      assert_includes(disable, 'workspace-codex@example-workspace.service')
      assert_includes(disable, 'workspace-tmux@example-workspace.service')
    end
  end

  def test_suspend_and_codex_reconciliation_refuse_unfinished_lifecycle_operations
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-pending.removal.json'), "{}\n")
      host.events.clear

      assert_equal(1, host.run('workspace-host', ['suspend']))
      assert_equal(1, host.run('workspace-host', ['reconcile-codex']))
      refute(host.events.any? { |event| event.first == :sessions_quiesced })
      refute_includes(host.events, [:consumers_restarted])
    end
  end

  private

  class CapturingHost < DevWorkspaceHost::Host
    attr_reader :captured

    private

    def exec_with_workspace(entry, command, *arguments)
      @captured = [@env.to_h.merge('DEV_WORKSPACE_NAME' => entry.fetch('name')), command, arguments]
    end
  end

  class ClusterHost < DevWorkspaceHost::Host
    attr_reader :captured

    private

    def system_env!(environment, command, *arguments)
      @captured = [environment, command, arguments]
    end
  end

  class GenerationClusterHost < ClusterHost
    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end
  end

  class GenerationMutationHost < DevWorkspaceHost::Host
    attr_reader :captured

    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end

    def suspend(argv)
      raise DevWorkspaceHost::Error, 'unexpected arguments' unless argv.empty?

      @captured = :suspended
    end
  end

  class CompatibilityLinkHost < DevWorkspaceHost::Host
    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end
  end

  class PortalArgumentHost < CompatibilityLinkHost
    attr_reader :execution

    private

    def exec(*arguments)
      @execution = arguments
    end

    def active_codex
      '/nix/store/codex/bin/codex'
    end

    def codex_version(_command)
      '0.152.1'
    end

    def find_command(name)
      "/usr/bin/#{name}"
    end
  end

  class FinalizeHost < DevWorkspaceHost::Host
    attr_reader :commands
    attr_accessor :cluster_active

    def initialize(**options)
      super
      @commands = []
      @cluster_active = true
    end

    private

    def capture_env!(_environment, command, *arguments)
      if File.basename(command).include?('dev-session')
        separator = arguments.index('--')
        request = arguments.drop(separator + 2)
        slug = request.find { |argument| !argument.start_with?('-') }
        "https://example-workspace.workspace.example.test/#{slug}/\n"
      else
        found = cluster_active && File.basename(command) == 'alpha-devcluster'
        JSON.generate('schema' => 1, 'found' => found)
      end
    end

    def system_env!(_environment, command, *arguments)
      @commands << [command, arguments]
    end
  end

  class UnregisterHost < DevWorkspaceHost::Host
    attr_reader :commands

    def initialize(**options)
      super
      @commands = []
    end

    private

    def system!(*argv)
      @commands << argv
    end

    def quiesce_sessions(_entries)
      []
    end
  end

  class BootstrappingHost < UnregisterHost
    private

    def capture!(*argv)
      if argv[0, 2] == ['nix', 'build']
        raise DevWorkspaceHost::Error, 'injected first switch failure'
      end

      super
    end
  end

  class QuiesceHost < DevWorkspaceHost::Host
    attr_reader :commands

    def initialize(**options)
      super
      @commands = []
    end

    private

    def capture_env!(environment, command, *arguments)
      @commands << [environment, command, arguments]
      "quiesced terminal: #{arguments[-2]}\n"
    end
  end

  class FailedUnregisterHost < DevWorkspaceHost::Host
    attr_reader :commands, :restored

    def initialize(**options)
      super
      @commands = []
    end

    private

    def quiesce_sessions(_entries)
      [:quiesced]
    end

    def restore_quiesced_sessions(sessions, **)
      @restored = sessions
      raise DevWorkspaceHost::Error, 'injected terminal recovery failure'
    end

    def system!(*argv)
      @commands << argv
      if argv[0, 4] == ['systemctl', '--user', 'disable', '--now']
        raise DevWorkspaceHost::Error, 'injected partial disable failure'
      elsif argv[0, 4] == ['systemctl', '--user', 'enable', '--now']
        raise DevWorkspaceHost::Error, 'injected unit recovery failure'
      end
    end
  end

  class PostCommitFailureRegistry < DevWorkspaceHost::Registry
    def unregister(name)
      existing = find(name)
      raise DevWorkspaceHost::Error, "workspace is not registered: #{name}" unless existing

      # Model a failure after the replacement became visible but before the
      # Registry instance updated its cached entries.
      send(:write, entries.reject { |entry| entry.fetch('name') == name })
      raise DevWorkspaceHost::Error,
            'injected failure after the registry replacement'
    end
  end

  class PostCommitFailureUnregisterHost < UnregisterHost
    attr_reader :restored

    private

    def registry
      @injected_registry ||= PostCommitFailureRegistry.new(@config)
    end

    def quiesce_sessions(_entries)
      [:quiesced]
    end

    def restore_quiesced_sessions(sessions, **)
      @restored = sessions
    end
  end

  class FailedLateUnregisterHost < DevWorkspaceHost::Host
    attr_reader :commands, :restored, :runtime_restore_attempted

    def initialize(**options)
      super
      @commands = []
    end

    private

    def quiesce_sessions(_entries)
      [:quiesced]
    end

    def system!(*argv)
      @commands << argv
      if argv == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        raise DevWorkspaceHost::Error, 'injected router failure'
      end
    end

    def restore_workspace_registration(_registry_path, _entry)
      {
        state: :absent,
        error: DevWorkspaceHost::Error.new('injected registration recovery failure')
      }
    end

    def restore_instance_runtime(_retired, _entry)
      @runtime_restore_attempted = true
      raise DevWorkspaceHost::Error, 'injected runtime recovery failure'
    end

    def restore_quiesced_sessions(sessions, **)
      @restored = sessions
    end
  end

  class AmbiguousRecoveryWriteRegistry < DevWorkspaceHost::Registry
    def register(**options)
      super
      raise DevWorkspaceHost::Error,
            'injected failure after the recovery replacement'
    end
  end

  class AmbiguousRecoveryWriteUnregisterHost < PostCommitFailureUnregisterHost
    private

    def open_registry(path)
      @registry_open_count ||= 0
      @registry_open_count += 1
      if @registry_open_count == 1
        AmbiguousRecoveryWriteRegistry.new(path)
      else
        DevWorkspaceHost::Registry.new(path)
      end
    end
  end

  class ReplacementDuringUnregisterHost < UnregisterHost
    def initialize(replacement:, **options)
      super(**options)
      @replacement = replacement
      @router_failed = false
    end

    private

    def system!(*argv)
      @commands << argv
      return unless argv == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
      return if @router_failed

      @router_failed = true
      DevWorkspaceHost::Registry.new(@config).register(
        name: 'example-workspace', root: @replacement,
        hostname: 'replacement.workspace.example.test', aliases: [], replace: false
      )
      raise DevWorkspaceHost::Error, 'injected router failure after replacement'
    end
  end

  class FailedOutput < StringIO
    def puts(*)
      raise Errno::EPIPE
    end
  end

  class TransitionHost < DevWorkspaceHost::Host
    attr_accessor :busy, :candidate, :fail_activation, :fail_links, :fail_restart, :fail_restore,
                  :fail_set_after_profile
    attr_reader :events

    def initialize(candidate:, busy:, **options)
      super(**options)
      @candidate = candidate
      @busy = busy
      @events = []
    end

    private

    # A real stable command invocation comes from the selected profile. Tests
    # reuse this host object across invocations, so follow the selected profile
    # to model the package that a new process would execute.
    def package_root
      File.symlink?(@profile) ? File.realpath(@profile) : super
    end

    def capture!(*argv)
      return "#{candidate}\n" if argv[0, 2] == ['nix', 'build']
      super
    end

    def system!(*argv)
      if argv[0, 3] == ['nix-env', '--profile', @profile]
        if argv[3] == '--set'
          generations = Dir["#{@profile}-*-link"].filter_map do |path|
            File.basename(path)[/-(\d+)-link\z/, 1]&.to_i
          end
          current = generations.max.to_i + 1
          generation = profile_generation_path(current)
          File.symlink(argv.fetch(4), generation)
          File.unlink(@profile) if File.symlink?(@profile)
          File.symlink(File.basename(generation), @profile)
          @events << [:profile_set, current]
          if fail_set_after_profile
            self.fail_set_after_profile = false
            raise DevWorkspaceHost::Error, 'injected post-commit profile selection failure'
          end
          return
        elsif argv[3] == '--rollback'
          target = previous_profile_generation
          File.unlink(@profile)
          File.symlink(File.basename(profile_generation_path(target)), @profile)
          @events << [:profile_rolled_back, target]
          return
        elsif argv[3] == '--switch-generation'
          target = Integer(argv.fetch(4), 10)
          File.unlink(@profile)
          File.symlink(File.basename(profile_generation_path(target)), @profile)
          @events << [:profile_selected, target]
          return
        elsif argv[3] == '--delete-generations'
          target = Integer(argv.fetch(4), 10)
          File.unlink(profile_generation_path(target))
          @events << [:profile_deleted, target]
          return
        end
      end
      if argv[0, 4] == ['systemctl', '--user', 'restart', 'workspace-router.service']
        @events << [:router_restarted]
      else
        @events << [:command, *argv]
      end
    end

    def root_codex(command, root)
      package = File.dirname(File.dirname(File.realpath(command)))
      FileUtils.mkdir_p(File.dirname(root))
      File.unlink(root) if File.symlink?(root)
      File.symlink(package, root)
    end

    def install_links(**)
      @events << [:links_installed]
      if fail_links
        self.fail_links = false
        raise DevWorkspaceHost::Error, 'injected link installation failure'
      end
    end

    def configure_user_services
      @events << [:configured]
    end

    def activate_installed(_command)
      configure_user_services
      if fail_activation
        self.fail_activation = false
        raise DevWorkspaceHost::Error, 'injected activation failure'
      end
      reconcile_codex_update(defer_busy: true)
    end

    def quiesce_sessions
      raise DevWorkspaceHost::Error, busy.join(', ') unless busy.empty?
      @events << [:sessions_quiesced]
      []
    end

    def wait_for_codex_sockets
      @events << [:codex_ready]
    end

    def restore_quiesced_sessions(_sessions, package: nil)
      @events << [:sessions_restored, package && File.realpath(package)]
      if fail_restore
        self.fail_restore = false
        raise DevWorkspaceHost::Error, 'injected restoration failure'
      end
    end

    def check_codex(command)
      @events << [:codex_checked, File.realpath(command)]
    end

    def registration_plan_for(_entry, package: package_root)
      DevWorkspaceHost::RegistrationPlan.new(
        package_root: File.realpath(package),
        argv: ['-c', 'agents.dw_transition.config_file=/nix/store/transition-role.toml'],
        digest: 'a' * 64, policy: 1,
        required_native_child_threads: 1, states: []
      )
    end

    def probe_codex_registration(codex, plan)
      @events << [:registration_probed, File.realpath(codex), plan.digest]
    end

    def busy_codex_sessions
      busy
    end

    def restart_codex_consumers
      @events << [:consumers_restarted]
      if fail_restart
        self.fail_restart = false
        raise DevWorkspaceHost::Error, 'injected consumer restart failure'
      end
      registration_plans.each do |entry, plan|
        write_registration_marker(entry, File.realpath(@system_codex), plan)
      end
    end
  end

  class RestorationHost < DevWorkspaceHost::Host
    attr_reader :invocations

    def initialize(fail_slug:, **options)
      super(**options)
      @fail_slug = fail_slug
      @invocations = []
    end

    private

    def codex_version(_command)
      '0.153.4'
    end

    def system_env!(environment, command, *arguments)
      @invocations << [environment, command, arguments]
      slug = arguments.fetch(-2)
      raise DevWorkspaceHost::Error, 'injected sync failure' if slug == @fail_slug
    end
  end

  class ActivationGuardHost < DevWorkspaceHost::Host
    attr_reader :configured

    def initialize(package:, **options)
      super(**options)
      @package = package
      @configured = false
    end

    private

    def package_root
      @package
    end

    def configure_user_services
      @configured = true
    end

    def reconcile_codex_update(defer_busy:)
      defer_busy
    end
  end

end
