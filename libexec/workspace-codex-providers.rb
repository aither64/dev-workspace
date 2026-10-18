# frozen_string_literal: true

require 'securerandom'

module DevWorkspaceHost
  module CodexProviderHost
    private

    PROVIDER_OPERATION_ID = /\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/
    PROVIDER_ID = /\A[a-z0-9][a-z0-9_-]{0,62}\z/

    def legacy_provider_command(argv)
      action = argv.shift
      name = argv.shift
      entry = registry.find(name.to_s)
      raise Error, 'provider operation requires a registered workspace' unless entry

      case action
      when 'native-args'
        raise Error, 'provider native-args takes only a workspace name' unless argv.empty?
        # Read-only projection used by a terminal helper whose caller already
        # owns transition admission, including exclusive worker restoration.
        require_current_package_generation!
        require_provider_admission!(entry)
        @out.puts JSON.generate('argv' => provider_argv(entry))
      when 'status'
        raise Error, 'provider status takes only a workspace name' unless argv.empty?
        @out.puts JSON.generate(provider_public_status(entry))
      when 'request'
        raise Error, 'provider request requires provider ID and operation UUID' unless argv.length == 2
        target, identity = argv
        raise Error, 'invalid provider operation UUID' unless identity.match?(PROVIDER_OPERATION_ID)
        with_current_generation_lock(mode: File::LOCK_SH) do
          catalog = provider_catalog
          raise Error, 'provider is unavailable' unless catalog.fetch('catalog').fetch('providers').any? { |p| p.fetch('id') == target }
          provider_locked(entry) do
            state = provider_state(entry, catalog)
            operation = state['operation']
            receipts = state['receipts'] ||= {}
            if (saved = receipts[identity])
              raise Error, 'provider request UUID already has a different target' unless saved['target'] == target
            else
              if operation && %w[waiting applying recovery-required].include?(operation.fetch('phase'))
                raise Error, 'another provider request is pending; cancel it before selecting a different target'
              end
              raise Error, 'provider request retention limit reached' if receipts.length >= 2048
              receipts[identity] = {'target' => target, 'catalogDigest' => catalog.fetch('digest')}
              now = Time.now.utc.iso8601
              state['operation'] = {
                'id' => identity, 'target' => target, 'catalogDigest' => catalog.fetch('digest'),
                'phase' => state.fetch('active') == target ? 'applied' : 'waiting',
                'blockers' => [], 'createdAt' => now, 'updatedAt' => now
              }
              provider_write_state(entry, state)
            end
          end
          system! 'systemctl', '--user', 'enable', '--now', provider_worker_unit(entry)
        end
        @out.puts JSON.generate(provider_public_status(entry))
      when 'cancel'
        raise Error, 'provider cancellation requires the exact operation UUID' unless argv.length == 1 && argv.first.match?(PROVIDER_OPERATION_ID)
        with_current_generation_lock(mode: File::LOCK_SH) do
          provider_locked(entry) do
            state = provider_state(entry)
            operation = state['operation']
            raise Error, 'provider operation identity changed' unless operation && operation.fetch('id') == argv.first
            if operation.fetch('phase') == 'waiting'
              provider_finish(state, 'canceled')
              provider_write_state(entry, state)
            elsif operation.fetch('phase') != 'canceled'
              raise Error, 'provider operation can no longer be canceled'
            end
          end
        end
        @out.puts JSON.generate(provider_public_status(entry))
      when 'recover'
        raise Error, 'provider recovery takes only a workspace name' unless argv.empty?
        with_current_generation_lock(mode: File::LOCK_SH) do
          system! 'systemctl', '--user', 'start', provider_worker_unit(entry)
        end
      else
        raise Error, 'provider operation must be status, request, cancel or recover'
      end
    end

    def provider_catalog
      path = @env.fetch('DEV_WORKSPACES_CODEX_PROVIDERS', '/etc/dev-workspaces/codex-providers.json')
      output = capture!(File.join(package_root, 'bin/workspace-portal'), 'codex-provider', 'catalog', '--catalog', path)
      raise Error, 'provider catalog exceeds bounds' if output.bytesize > 128 * 1024
      data = JSON.parse(output)
      raise Error, 'provider catalog helper returned invalid data' unless data.is_a?(Hash) && data.keys.sort == %w[catalog digest] && data.fetch('digest').match?(SHA256_HEX)
      data
    end

    def provider_directory(entry)
      File.join(@state, 'providers', entry.fetch('name'))
    end

    def provider_state_path(entry)
      File.join(provider_directory(entry), 'state.json')
    end

    def provider_locked(entry)
      directory = provider_directory(entry)
      FileUtils.mkdir_p(directory, mode: 0o700)
      File.open(File.join(directory, 'request.lock'), File::RDWR | File::CREAT, 0o600) do |lock|
        lock.flock(File::LOCK_EX)
        yield
      end
    end

    def provider_state(entry, catalog = nil)
      path = provider_state_path(entry)
      unless File.exist?(path)
        catalog ||= provider_catalog
        return {
          'schema' => 1, 'workspace' => entry.fetch('name'), 'root' => entry.fetch('root'),
          'socket' => instance_runtime(entry).fetch(:codex),
          'active' => catalog.fetch('catalog').fetch('initialProvider')
        }
      end
      data = JSON.parse(read_private_json_file!(path, 'workspace provider state', maximum: 1024 * 1024))
      unless data.is_a?(Hash) && (data.keys - %w[schema workspace root socket active operation receipts sessions]).empty? &&
             [1, 2].include?(data['schema']) && data['workspace'] == entry.fetch('name') &&
             data['root'] == entry.fetch('root') && data['socket'] == instance_runtime(entry).fetch(:codex) &&
             data['active'].is_a?(String) && data['active'].match?(PROVIDER_ID)
        raise Error, 'workspace provider state has a different ownership identity'
      end
      if (receipts = data['receipts'])
        unless receipts.is_a?(Hash) && receipts.length <= 2048 && receipts.all? { |id, receipt|
          id.match?(PROVIDER_OPERATION_ID) && receipt.is_a?(Hash) && (receipt.keys - %w[catalogDigest target session inherit]).empty? &&
            (!receipt.key?('inherit') || [true, false].include?(receipt['inherit']) && (!receipt['inherit'] || receipt['session'])) &&
            (!receipt.key?('session') || receipt['session'].is_a?(String) && receipt['session'].match?(PROVIDER_ID)) &&
            receipt['target'].is_a?(String) && receipt['target'].match?(PROVIDER_ID) &&
            receipt['catalogDigest'].is_a?(String) && receipt['catalogDigest'].match?(SHA256_HEX)
        }
          raise Error, 'workspace provider request receipts are invalid'
        end
      end
      if data['sessions']
        unless data['schema'] == 2 && data['sessions'].is_a?(Hash) && data['sessions'].length <= 4096 &&
               data['sessions'].all? { |slug, selection|
                 slug.match?(PROVIDER_ID) && selection.is_a?(Hash) &&
                   (selection.keys - %w[provider rootThreadId creationId]).empty? &&
                   selection['provider'].is_a?(String) && selection['provider'].match?(PROVIDER_ID) &&
                   (!selection.key?('rootThreadId') || provider_thread_identity?(selection['rootThreadId'])) &&
                   (!selection.key?('creationId') || selection['creationId'].is_a?(String) && selection['creationId'].match?(SHA256_HEX)) &&
                   (selection.key?('rootThreadId') || selection.key?('creationId'))
               }
          raise Error, 'invalid session provider selections'
        end
      end
      if (operation = data['operation'])
        unless operation.is_a?(Hash) && operation['id'].is_a?(String) && operation['id'].match?(PROVIDER_OPERATION_ID) &&
               %w[waiting applying applied canceled failed recovery-required].include?(operation['phase'])
          raise Error, 'workspace provider operation is invalid'
        end
        validate_scoped_provider_operation!(data) if operation['strategy']
      end
      if (operation = data['operation'])
        control_path = File.join(provider_directory(entry), "#{operation.fetch('id')}.control.json")
        if File.exist?(control_path)
          control = JSON.parse(read_private_json_file!(control_path, 'provider control', maximum: 4096))
          unless control.is_a?(Hash) && (control.keys - %w[operation cancelRequested dismissed]).empty? &&
                 control['operation'] == operation['id'] &&
                 %w[cancelRequested dismissed].all? { |key| !control.key?(key) || [true, false].include?(control[key]) }
            raise Error, 'invalid provider control identity'
          end
          operation.merge!(control.slice('cancelRequested', 'dismissed'))
        end
      end
      data
    end

    def provider_write_state(entry, state)
      persisted = state.dup
      if (operation = state['operation'])
        control = operation.slice('cancelRequested', 'dismissed')
        unless control.empty?
          control['operation'] = operation.fetch('id')
          atomic_private_file(File.join(provider_directory(entry), "#{operation.fetch('id')}.control.json"), JSON.generate(control))
        end
        persisted['operation'] = operation.reject { |key, _| %w[cancelRequested dismissed].include?(key) }
      end
      data = JSON.generate(persisted) + "\n"
      raise Error, 'workspace provider state exceeds bounds' if data.bytesize > 1024 * 1024
      atomic_private_file(provider_state_path(entry), data)
    end

    def provider_public_status(entry)
      catalog = provider_catalog
      state = provider_state(entry, catalog)
      result = {
        'providers' => catalog.fetch('catalog').fetch('providers').map { |p| p.slice('id', 'label', 'kind') },
        'active' => state.fetch('active')
      }
      result['operation'] = provider_operation_view(entry, state.fetch('operation')) if state['operation']
      result['sessions'] = state.fetch('sessions', {}).transform_values { |selection| selection.slice('provider', 'rootThreadId') }
      result
    end

    def provider_reserved?(entry)
      return false unless File.exist?(provider_state_path(entry))
      %w[applying recovery-required].include?(provider_state(entry).dig('operation', 'phase'))
    end

    def require_provider_admission!(entry)
      raise Error, 'workspace provider transition is reserved; wait or run workspace-host provider recover' if provider_reserved?(entry)
    end

    def require_no_pending_provider!(entries)
      entries.each do |entry|
        next unless File.exist?(provider_state_path(entry))
        phase = provider_state(entry).dig('operation', 'phase')
        raise Error, 'cancel the waiting provider request before suspending or unregistering its workspace' if phase == 'waiting'
      end
    end

    def require_compatible_provider_state!(package)
      creation_policy = registry.entries.map { |entry| portal_creation_provider_policy(entry) }.max || 0
      scoped_creations = creation_policy.positive?
      return unless scoped_creations || registry.entries.any? { |entry| File.exist?(provider_state_path(entry)) }
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      data = JSON.parse(File.binread(path, 64 * 1024 + 1))
      required = scoped_creations || registry.entries.any? { |entry| File.exist?(provider_state_path(entry)) && provider_state(entry)['schema'] == 2 } ? 2 : 1
      supported = if required == 2
                    data['codexProviderScopedStateSchema'] == 2 && data['codexProviderScopedTransitionPolicy'] == 1
                  else
                    data['codexProviderStateSchema'] == 1 && data['codexProviderTransitionPolicy'] == 1
                  end
      if creation_policy == 2 && data['codexProviderForkCreationPolicy'] != 1
        raise Error, 'target workspace package cannot load retained fork provider receipts'
      end
      unless supported
        raise Error, 'target workspace package cannot load retained Codex provider state'
      end
    rescue JSON::ParserError, SystemCallError
      raise Error, 'target workspace package has no supported Codex provider contract'
    end

    def provider_argv(entry, selected: nil)
      path = @env.fetch('DEV_WORKSPACES_CODEX_PROVIDERS', '/etc/dev-workspaces/codex-providers.json')
      return [] unless File.exist?(path) || File.exist?(provider_state_path(entry))
      catalog = provider_catalog.fetch('catalog')
      selected ||= provider_state(entry).fetch('active')
      raise Error, 'saved Codex provider is unavailable' unless catalog.fetch('providers').any? { |p| p.fetch('id') == selected }
      arguments = ['-c', "model_provider=#{JSON.generate(selected)}"]
      catalog.fetch('providers').each do |p|
        next unless p.fetch('kind') == 'codex-lb'
        prefix = "model_providers.#{p.fetch('id')}"
        values = {
          'name' => p.fetch('label'), 'base_url' => p.fetch('baseURL'),
          'wire_api' => 'responses', 'supports_websockets' => false,
          'auth.command' => find_command('cat'), 'auth.args' => [p.fetch('tokenFile')]
        }
        values.each { |key, value| arguments.concat(['-c', "#{prefix}.#{key}=#{JSON.generate(value)}"]) }
      end
      arguments
    end

    def provider_worker_unit(entry)
      "workspace-codex-provider@#{entry.fetch('name')}.service"
    end

    def provider_finish(state, phase, message = nil)
      state.fetch('operation')['phase'] = phase
      state.fetch('operation')['updatedAt'] = Time.now.utc.iso8601
      state.fetch('operation')['message'] = message if message
      state.fetch('operation')['blockers'] = []
    end

    def provider_helper(entry, action, snapshot: nil, target: nil, socket: nil)
      arguments = [
        'codex-provider', action, '--workspace', entry.fetch('root'),
        '--state-root', @state, '--socket', socket || instance_runtime(entry).fetch(:codex),
        '--host-profile', @profile
      ]
      arguments.concat(['--snapshot', snapshot]) if snapshot
      arguments.concat(['--target', target]) if target
      output = capture!(File.join(package_root, 'bin/workspace-portal'), *arguments)
      raise Error, 'provider helper response exceeds bounds' if output.bytesize > 16 * 1024 * 1024
      JSON.parse(output)
    end

    def provider_thread_environment(entry, slug, member_address = nil)
      runtime = instance_runtime(entry)
      workspace = entry.fetch('root')
      base_url = "https://#{entry.fetch('hostname')}"
      environment = {
        'DEV_SESSION_SLUG' => slug, 'DEV_SESSION_WORKSPACE' => workspace,
        'DEV_SESSION_WORK_DIR' => File.join(workspace, 'work', slug),
        'DEV_SESSION_WORKTREES_DIR' => File.join(workspace, 'worktrees', slug),
        'DEV_SESSION_PORTAL_BASE_URL' => base_url, 'DEV_SESSION_URL' => "#{base_url}/#{slug}/",
        'DEV_SESSION_AUTHORITY_DIR' => runtime.fetch(:authority),
        'DEV_SESSION_TMUX_SOCKET' => runtime.fetch(:tmux),
        'DEV_SESSION_CODEX' => active_codex, 'DEV_SESSION_CODEX_SOCKET' => runtime.fetch(:codex),
        'DEV_SESSION_CODEX_VERSION' => codex_version(active_codex),
        'DEV_SESSION_PORTAL_COMMAND' => File.join(package_root, 'bin/workspace-portal'),
        'DEV_SESSION_REQUIRE_RUNTIME' => '1'
      }
      environment['DEV_SESSION_MEMBER_ADDRESS'] = member_address if member_address && !member_address.empty?
      environment
    end

    def require_provider_restart_safe!(entry, inventory = nil)
      unit = "workspace-codex@#{entry.fetch('name')}.service"
      properties = capture!('systemctl', '--user', 'show', unit, '--property=MainPID,ControlGroup').lines.to_h do |line|
        line.strip.split('=', 2)
      end
      pid = Integer(properties.fetch('MainPID'), 10)
      group = properties.fetch('ControlGroup')
      raise Error, 'native daemon process identity is unavailable' unless pid.positive? && group.start_with?('/') && !group.split('/').include?('..')
      processes = provider_cgroup_processes("/sys/fs/cgroup#{group}")
      unless processes.include?(pid) && provider_process_binary(pid) == provider_native_binary &&
             (processes - [pid]).all? { |child| provider_owned_idle_child?(entry, pid, child, inventory) }
        raise Error, 'Codex service has additional or unproved processes; restarting it would interrupt unrelated work'
      end
    end

    def provider_owned_idle_child?(entry, parent, pid, inventory)
      return false unless inventory && inventory.fetch('blockers').empty? && provider_process_parent(pid) == parent
      executable = provider_process_binary(pid)
      arguments = provider_process_arguments(pid)
      code_mode = File.join(File.dirname(provider_native_binary), 'codex-code-mode-host')
      return true if executable == code_mode && arguments == [code_mode]
      commands = [File.join(package_root, 'bin/workspace-portal')]
      marker = load_registration_marker(entry)
      commands << File.join(marker.fetch('launch').fetch('package_root'), 'bin/workspace-portal') if marker.is_a?(Hash)
      return false unless commands.include?(executable)
      if marker.is_a?(Hash)
        launch = marker.fetch('launch')
        invocation = reporting_server_invocation(entry, package: launch.fetch('package_root'))
        # Daemon-start servers have no legacy per-thread MCP policy. Their
        # exact validated launch marker proves this owned host helper instead.
        return true if executable == invocation.first && arguments == invocation &&
          valid_reporting_registration_argv?(launch.fetch('argv'), entry, launch.fetch('package_root'))
      end
      inventory.fetch('threads').any? do |thread|
        server = thread.dig('settings', 'policy', 'mcpServer')
        thread['memberAddress'] && server && arguments == [executable, *server.fetch('Args')]
      end
    end

    def provider_process_parent(pid)
      Integer(File.read("/proc/#{pid}/status").lines.find { |line| line.start_with?('PPid:') }.split.last, 10)
    end

    def provider_process_arguments(pid)
      data = File.binread("/proc/#{pid}/cmdline", 64 * 1024 + 1)
      raise Error, 'native child arguments exceed bounds' if data.bytesize > 64 * 1024
      data.split("\0")
    end

    def provider_cgroup_processes(directory)
      # systemd unit paths contain literal backslashes; keep the root out of
      # the glob pattern so they are not interpreted as escape sequences.
      Dir.glob('**/cgroup.procs', base: directory).flat_map do |path|
        File.readlines(File.join(directory, path)).map { |line| Integer(line.strip, 10) }
      end.uniq
    end

    def provider_process_binary(pid)
      File.realpath("/proc/#{pid}/exe")
    end

    def provider_native_binary
      command = File.realpath(active_codex)
      File.realpath(File.join(File.dirname(File.dirname(command)), 'libexec/codex/bin/codex'))
    end

    def provider_restore_terminals(entry, snapshot)
      path = snapshot + '.terminals'
      return unless File.exist?(path)
      data = JSON.parse(read_private_json_file!(path, 'provider terminal snapshot', maximum: 1024 * 1024))
      runtime = instance_runtime(entry)
      unless data['schema'] == 1 && data['root'] == entry.fetch('root') && data['workspace'] == entry.fetch('name') && data['socket'] == runtime.fetch(:codex)
        raise Error, 'provider terminal snapshot ownership changed'
      end
      keys = %w[id name mark slug workspace environment_slug socket_path codex_thread_id codex_socket_path codex_client_version codex_pane_id identity_token]
      data.fetch('terminals').each_with_index do |record, index|
        session = record.fetch('session')
        yield session.fetch('slug'), index, data.fetch('terminals').length if block_given?
        unless session.keys.sort == keys.sort && session.fetch('id').match?(/\A\$[0-9]+\z/) &&
               session.fetch('workspace') == entry.fetch('root') && session.fetch('socket_path') == runtime.fetch(:tmux)
          raise Error, 'invalid retained terminal identity'
        end
        socket_stat = File.stat(runtime.fetch(:tmux))
        unless record.fetch('socketIdentity') == [socket_stat.dev, socket_stat.ino]
          raise Error, 'retained terminal socket generation changed'
        end
        actual = capture!('tmux', '-S', runtime.fetch(:tmux), 'display-message', '-p', '-t', "#{session.fetch('id')}:", '-F', RUNTIME_CONTRACT.fetch('tmuxMetadata').fetch('authorityFormat').join("\t")).chomp.split("\t", -1)
        raise Error, 'retained terminal ownership changed' unless actual == keys.map { |key| session.fetch(key).to_s }
        restore_quiesced_sessions([[entry, session.fetch('slug')]], provider_terminal_snapshot: path)
      end
    end

    def run_provider_worker(argv)
      entry = instance_argument(argv, 'run-provider-worker')
      loop do
        state = provider_state(entry)
        operation = state['operation']
        unless operation && %w[waiting applying recovery-required].include?(operation.fetch('phase'))
          provider_worker_pause
          next
        end
        if operation['strategy'] == 'scoped-v1'
          begin
            with_current_generation_lock(mode: File::LOCK_SH | File::LOCK_NB) { provider_run_scoped(entry) }
          rescue Error => error
            raise unless error.message == 'workspace package transition is in progress; retry shortly'
            provider_locked(entry) do
              current = provider_state(entry)
              if current.dig('operation', 'id') == operation['id'] && current.dig('operation', 'phase') == 'waiting'
                current['operation']['stage'] = 'waiting-package'
                current['operation']['blockers'] = ['A workspace package update is running. The provider switch will start when it finishes.']
                provider_write_state(entry, current)
              end
            end
          end
          provider_worker_pause
          next
        end
        if operation.fetch('phase') != 'waiting'
          with_current_generation_lock { provider_recover(entry) }
          provider_worker_pause
          next
        end
        # Waiting observations share the normal generation lock and never
        # reserve the workspace against new user work. The exclusive pass
        # below repeats every proof only when the first pass found it idle.
        inventory = nil
        blockers = []
        with_current_generation_lock(mode: File::LOCK_SH) do
          begin
            require_no_unfinished_lifecycle_operations!([entry], operation: 'provider switch')
            inventory = provider_helper(entry, 'inventory')
            blockers = inventory.fetch('blockers')
            require_provider_restart_safe!(entry, inventory) if blockers.empty?
          rescue StandardError
            blockers = ['Workspace activity or service ownership could not be verified.']
          end
          provider_locked(entry) do
            current = provider_state(entry)
            next unless current.dig('operation', 'id') == operation.fetch('id') && current.dig('operation', 'phase') == 'waiting'
            if provider_catalog.fetch('digest') != operation.fetch('catalogDigest')
              provider_finish(current, 'failed', 'Provider configuration changed. Select the provider again.')
            else
              current.fetch('operation')['blockers'] = blockers
              current.fetch('operation')['updatedAt'] = Time.now.utc.iso8601
            end
            provider_write_state(entry, current)
          end
        end
        unless blockers.empty?
          provider_worker_pause
          next
        end
        with_current_generation_lock do
          provider_locked(entry) do
            state = provider_state(entry)
            next unless state.dig('operation', 'id') == operation.fetch('id') && state.dig('operation', 'phase') == 'waiting'
            if provider_catalog.fetch('digest') != operation.fetch('catalogDigest')
              provider_finish(state, 'failed', 'Provider configuration changed. Select the provider again.')
              provider_write_state(entry, state)
              next
            end
            begin
              require_no_unfinished_lifecycle_operations!([entry], operation: 'provider switch')
              inventory = provider_helper(entry, 'inventory')
              blockers = inventory.fetch('blockers')
              require_provider_restart_safe!(entry, inventory) if blockers.empty?
            rescue StandardError
              blockers = ['Workspace activity or service ownership could not be verified.']
            end
            unless blockers.empty?
              state.fetch('operation')['blockers'] = blockers
              state.fetch('operation')['updatedAt'] = Time.now.utc.iso8601
              provider_write_state(entry, state)
              next
            end
            snapshot = File.join(provider_directory(entry), "#{operation.fetch('id')}.snapshot.json")
            inventory.fetch('threads').each do |thread|
              thread['environment'] = provider_thread_environment(entry, thread.fetch('slug'), thread['memberAddress'])
            end
            atomic_private_file(snapshot, JSON.generate(inventory))
            begin
              provider_helper(entry, 'preflight', snapshot:, target: operation.fetch('target'))
            rescue StandardError
              provider_finish(state, 'failed', 'Target provider authentication, connectivity or retained models could not be verified.')
              provider_write_state(entry, state)
              next
            end
            state.fetch('operation')['snapshot'] = snapshot
            state.fetch('operation')['previousActive'] = state.fetch('active')
            state.fetch('operation')['stage'] = 'capturing'
            state.fetch('operation')['phase'] = 'applying'
            state.fetch('operation')['updatedAt'] = Time.now.utc.iso8601
            provider_write_state(entry, state)
            provider_apply(entry, state, snapshot)
          end
        end
        # Stay available after terminal phases. A request accepted while this
        # service is active must not miss an exiting worker's final observation.
        provider_worker_pause
      end
    end

    def provider_worker_pause = sleep(0.5)

    def provider_private_server(entry, provider, process_group: true)
      Dir.mktmpdir('workspace-provider-', instance_runtime(entry).fetch(:root)) do |directory|
        socket = File.join(directory, 'app-server.sock')
        plan = registration_plan_for(entry, package: package_root)
        pid = Process.spawn(
          File.realpath(active_codex), *plan.argv, *provider_argv(entry, selected: provider),
          'app-server', '--listen', "unix://#{socket}",
          out: File::NULL, err: File::NULL, pgroup: process_group
        )
        begin
          deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 30
          until File.socket?(socket)
            raise Error, 'private native App Server exited' if Process.waitpid(pid, Process::WNOHANG)
            raise Error, 'private native App Server did not create its socket' if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
            sleep 0.05
          end
          yield socket
        ensure
          Process.kill('TERM', process_group ? -pid : pid) rescue nil
          Process.waitpid(pid) rescue nil
        end
      end
    end

    def provider_failure_class(error)
      name = error.class.name
      %w[DevWorkspaceHost::Error RuntimeError ArgumentError TypeError KeyError
         NoMethodError NameError IOError EOFError JSON::ParserError
         Errno::ENOENT Errno::EACCES Errno::EPERM Errno::ESRCH Errno::ECHILD
         Errno::EPIPE Errno::ECONNREFUSED Errno::ECONNRESET Errno::ETIMEDOUT
         Errno::ENOSPC Errno::EIO Errno::EINVAL Errno::EEXIST Errno::ENOTDIR].include?(name) ? name : 'OtherError'
    rescue Exception
      'OtherError'
    end

    def provider_failure_diagnostic(operation, direction, diagnostic, error)
      # Failure-only projection: never include messages or helper argv/output.
      sources = {
        __FILE__ => 'workspace-codex-providers.rb',
        File.join(__dir__, 'workspace-host') => 'workspace-host',
        File.join(__dir__, '.workspace-host-wrapped') => 'workspace-host'
      }
      frames = Array(error.backtrace_locations).filter_map do |frame|
        label = sources[frame.absolute_path || frame.path]
        {source: label, line: frame.lineno} if label && frame.lineno.between?(1, 1_000_000)
      end.first(4)
      id = operation['id']
      stage = operation['stage']
      elapsed = ((Process.clock_gettime(Process::CLOCK_MONOTONIC) - diagnostic.fetch(:started)) * 1000).to_i
      errno = error.is_a?(SystemCallError) ? error.errno : nil
      record = {
        operation: id.is_a?(String) && id.bytesize == 36 && id.match?(PROVIDER_OPERATION_ID) ? id : 'unavailable',
        direction: %w[forward recovery].include?(direction) ? direction : 'unavailable',
        stage: %w[capturing quiescing daemon-stop shared-start].include?(stage) ? stage : 'unavailable',
        step: diagnostic.fetch(:step), elapsedMs: elapsed.clamp(0, 86_400_000),
        exception: provider_failure_class(error),
        errno: errno.is_a?(Integer) && errno.between?(0, 65535) ? errno : 'unavailable',
        frames:
      }
      record[:readiness] = diagnostic[:readiness] if diagnostic[:readiness]
      line = "PROVIDER_FAILURE #{JSON.generate(record)}\n"
      @err.write(line) if line.bytesize <= 2048
    rescue Exception
      # Formatting or a broken stderr must not affect compensation/state writes.
      nil
    end

    def provider_apply(entry, state, snapshot)
      operation = state.fetch('operation')
      unit = "workspace-codex@#{entry.fetch('name')}.service"
      diagnostic = {step: 'capture', started: Process.clock_gettime(Process::CLOCK_MONOTONIC)}
      begin
        captured = provider_helper(entry, 'capture', snapshot:, target: operation.fetch('target'))
        diagnostic[:step] = 'capture-write'
        atomic_private_file(snapshot, JSON.generate(captured))
        diagnostic[:step] = 'quiescing-write'
        operation['stage'] = 'quiescing'
        provider_write_state(entry, state)
        diagnostic[:step] = 'terminal-snapshot'
        atomic_private_file(snapshot + '.terminals', JSON.generate('schema' => 1, 'workspace' => entry.fetch('name'), 'root' => entry.fetch('root'), 'socket' => instance_runtime(entry).fetch(:codex), 'terminals' => []))
        diagnostic[:step] = 'quiesce'
        quiesce_sessions([entry], provider_terminal_snapshot: snapshot + '.terminals')
        diagnostic[:step] = 'inventory'
        final_inventory = provider_helper(entry, 'inventory')
        raise Error, 'workspace became active before native daemon stop' unless final_inventory.fetch('blockers').empty?
        diagnostic[:step] = 'restart-safety'
        require_provider_restart_safe!(entry, final_inventory)
        diagnostic[:step] = 'daemon-stop-write'
        operation['stage'] = 'daemon-stop'
        provider_write_state(entry, state)
        diagnostic[:step] = 'daemon-stop'
        system! 'systemctl', '--user', 'stop', unit
        diagnostic[:step] = 'private-start'
        provider_private_server(entry, operation.fetch('target')) do |socket|
          diagnostic[:step] = 'private-resume'
          provider_helper(entry, 'resume', snapshot:, target: operation.fetch('target'), socket:)
        end
        diagnostic[:step] = 'shared-selection-write'
        state['active'] = operation.fetch('target')
        operation['stage'] = 'shared-start'
        provider_write_state(entry, state)
        diagnostic[:step] = 'shared-start'
        system! 'systemctl', '--user', 'start', unit
        diagnostic[:step] = 'shared-readiness'
        provider_wait_shared_server(entry, diagnostic:)
        diagnostic[:step] = 'verify'
        provider_helper(entry, 'verify', snapshot:, target: operation.fetch('target'))
        diagnostic[:step] = 'applied-write'
        provider_finish(state, 'applied', 'Provider changed. Terminal restoration is pending; run dev-session sync if interrupted.')
        provider_write_state(entry, state)
        begin
          diagnostic[:step] = 'terminal-restore'
          provider_restore_terminals(entry, snapshot)
          operation.delete('message')
          diagnostic[:step] = 'terminal-restored-write'
          provider_write_state(entry, state)
        rescue StandardError
          operation['message'] = 'Provider changed. Some terminal clients need dev-session sync.'
          diagnostic[:step] = 'terminal-notice-write'
          provider_write_state(entry, state)
        end
      rescue StandardError => error
        provider_failure_diagnostic(operation, 'forward', diagnostic, error)
        provider_finish(state, 'recovery-required', 'Provider switch needs recovery. Run workspace-host provider recover for this workspace.')
        provider_write_state(entry, state)
        provider_recover(entry)
      end
    end

    def provider_recover(entry)
      state = provider_state(entry)
      operation = state.fetch('operation')
      snapshot = operation.fetch('snapshot')
      expected = File.join(provider_directory(entry), "#{operation.fetch('id')}.snapshot.json")
      raise Error, 'provider recovery snapshot identity changed' unless snapshot == expected
      data = JSON.parse(read_private_json_file!(snapshot, 'provider recovery snapshot', maximum: 16 * 1024 * 1024))
      unit = "workspace-codex@#{entry.fetch('name')}.service"
      # Before capture finished no routing mutation was possible. Retain the
      # original daemon; retries can still safely finish its settings snapshot.
      if %w[capturing quiescing].include?(operation['stage'])
        provider_finish(state, 'failed', 'Routing was unchanged. Terminal restoration is pending; run dev-session sync if interrupted.')
        provider_write_state(entry, state)
        begin
          provider_restore_terminals(entry, snapshot)
          operation['message'] = 'Provider switch stopped before routing changed.'
          provider_write_state(entry, state)
        rescue StandardError
          operation['message'] = 'Routing was unchanged. Some terminal clients need dev-session sync.'
          provider_write_state(entry, state)
        end
        return
      end
      unless data.fetch('threads').all? { |thread| thread['original'] }
        raise Error, 'provider recovery settings snapshot is incomplete'
      end
      diagnostic = {step: 'running-state', started: Process.clock_gettime(Process::CLOCK_MONOTONIC)}
      begin
        running = capture!('systemctl', '--user', 'show', unit, '--property=MainPID', '--value').strip.to_i.positive?
        if running
          diagnostic[:step] = 'inventory'
          inventory = provider_helper(entry, 'inventory')
          diagnostic[:step] = 'restart-safety'
          require_provider_restart_safe!(entry, inventory)
        end
        diagnostic[:step] = 'daemon-stop'
        system! 'systemctl', '--user', 'stop', unit
        diagnostic[:step] = 'previous-selection'
        old = operation.fetch('previousActive')
        diagnostic[:step] = 'private-start'
        provider_private_server(entry, old) do |socket|
          diagnostic[:step] = 'private-resume'
          provider_helper(entry, 'resume', snapshot:, socket:)
        end
        diagnostic[:step] = 'shared-selection-write'
        state['active'] = old
        operation['stage'] = 'shared-start'
        provider_write_state(entry, state)
        diagnostic[:step] = 'shared-start'
        system! 'systemctl', '--user', 'start', unit
        diagnostic[:step] = 'shared-readiness'
        provider_wait_shared_server(entry, diagnostic:)
        diagnostic[:step] = 'verify'
        provider_helper(entry, 'verify', snapshot:)
        diagnostic[:step] = 'failed-write'
        provider_finish(state, 'failed', 'Original routing was restored. Terminal restoration is pending; run dev-session sync if interrupted.')
        provider_write_state(entry, state)
      rescue StandardError => error
        provider_failure_diagnostic(operation, 'recovery', diagnostic, error)
        provider_finish(state, 'recovery-required', 'Recovery could not be verified. Workspace writes remain blocked; retry provider recovery.')
        provider_write_state(entry, state)
        raise Error, 'provider recovery could not be verified'
      end
      begin
        provider_restore_terminals(entry, snapshot)
        operation['message'] = 'Provider switch failed. Original routing was restored.'
        provider_write_state(entry, state)
      rescue StandardError
        operation['message'] = 'Original routing was restored. Some terminal clients need dev-session sync.'
        provider_write_state(entry, state)
      end
    end

    def provider_wait_shared_server(entry, diagnostic: nil)
      unit = "workspace-codex@#{entry.fetch('name')}.service"
      socket = instance_runtime(entry).fetch(:codex)
      deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 30
      readiness = {mainPID: 'unavailable', activeState: 'unavailable', executableMatch: 'unavailable', socketPresent: 'unavailable', healthError: 'unavailable'}
      diagnostic[:readiness] = readiness if diagnostic
      loop do
        properties = capture!('systemctl', '--user', 'show', unit, '--property=MainPID,ActiveState').lines.to_h { |line| line.strip.split('=', 2) }
        readiness.merge!(mainPID: 'unavailable', activeState: 'unavailable', executableMatch: 'unavailable', socketPresent: 'unavailable')
        readiness[:mainPID] = properties['MainPID'].to_i if properties['MainPID'].to_s.match?(/\A[0-9]{1,10}\z/)
        readiness[:activeState] = properties['ActiveState'] if %w[active activating inactive deactivating failed reloading maintenance refreshing].include?(properties['ActiveState'])
        raise Error, 'shared native App Server exited during startup' unless %w[active activating].include?(properties['ActiveState']) && properties['MainPID'].to_i.positive?
        begin
          readiness[:executableMatch] = provider_process_binary(properties.fetch('MainPID').to_i) == provider_native_binary
        rescue Errno::EACCES, Errno::ENOENT
          # A freshly execing service can transiently deny /proc/PID/exe.
          # Keep the bounded deadline and require the exact executable later.
          readiness[:executableMatch] = 'unavailable'
        end
        if readiness[:executableMatch] == true && (readiness[:socketPresent] = File.socket?(socket))
          begin
            provider_helper(entry, 'health')
            return
          rescue StandardError => error
            readiness[:healthError] = provider_failure_class(error)
            # A socket pathname alone does not prove protocol readiness.
          end
        end
        raise Error, 'shared native App Server did not become ready' if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
        sleep 0.1
      end
    end
  end
end

require_relative 'workspace-provider-sessions'
