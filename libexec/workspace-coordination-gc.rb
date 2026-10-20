# frozen_string_literal: true

require 'set'

module WorkspaceCoordination
  # Separate bookkeeping leaves immutable snapshot IDs and the ledger readable
  # by earlier generations. Inspection never extends a preparation's lifetime.
  class Retention
    TTL = 7 * 86_400

    def initialize(store)
      @store = store
      @path = File.join(store.root, 'retention.json')
    end

    def read
      return {'schema' => 1, 'preparations' => {}, 'pending' => {}, 'orphans' => {}} unless File.exist?(@path)
      value = WorkspaceCoordination.private_read(@path)
      WorkspaceCoordination.keys!(value, %w[schema preparations pending orphans])
      unless value['schema'] == 1 && value['preparations'].is_a?(Hash) && value['pending'].is_a?(Hash) && value['orphans'].is_a?(Hash)
        raise Error, 'unsupported coordination retention bookkeeping'
      end
      value['preparations'].each do |id, item|
        unless id.match?(/\Asha256:[0-9a-f]{64}\z/) && item.keys.sort == %w[bindings expiresAt] &&
               item['expiresAt'].is_a?(Integer) && item['bindings'].is_a?(String)
          raise Error, 'invalid preparation retention record'
        end
      end
      value
    end

    def write(value)
      WorkspaceCoordination.atomic(@path, value, limit: MAX_LEDGER)
    end

    def bindings(id, ledger)
      values = ledger['jobs'].values.filter_map do |job|
        snapshots = [job['snapshot']] + job['history'].filter_map { |item| item['snapshot'] }
        count = snapshots.count { |item| item['id'] == id }
        [job['id'], count] if count.positive?
      end
      WorkspaceCoordination.digest(values.sort)
    end

    def register(id)
      @store.lock('retention') do
        value = read
        value['preparations'][id] = {'expiresAt' => Time.now.to_i + TTL, 'bindings' => bindings(id, @store.read)}
        write(value)
      end
    end

    def live?(id, ledger, value = read)
      item = value['preparations'][id]
      item && item['expiresAt'] > Time.now.to_i && item['bindings'] == bindings(id, ledger)
    end

    def require_fresh!(id)
      value = read
      return unless value['preparations'].key?(id) # Legacy preparation; the collector grants adoption grace.
      ledger = @store.read
      # Already queued work keeps its payloads irrespective of lease age. It
      # must remain possible to approve or refresh that retained operation.
      return if ledger['jobs'].values.any? do |job|
        !TERMINAL.include?(job['state']) &&
          ([job['snapshot']] + job['history'].filter_map { |item| item['snapshot'] }).any? { |item| item['id'] == id }
      end
      raise Error.new('preparation expired or was consumed; prepare this operation again', 3) unless live?(id, ledger, value)
    end
  end

  # Caller shares generation admission and exclusively holds the payload
  # preparation guard. Session creation can proceed during a sweep; package
  # switches and payload preparation remain excluded. Workers may finish, but
  # their claims remain roots and a changed ledger stops the current sweep.
  class Collector
    SOURCE = /\A[0-9a-f]{40}\z/
    DIGEST = /\A[0-9a-f]{64}\z/

    def initialize(engine)
      @engine = engine
      @store = engine.store
      @retention = Retention.new(@store)
      @sources = File.join(@store.root, 'sources')
    end

    def call(apply: false)
      @notification_locks = Set.new
      with_integration_exclusion(Dir[File.join(@store.root, 'integrations', '*.json')].sort, apply: apply) do
        with_notification_exclusion(@store.read['owners'].keys.sort) { sweep(apply: apply) }
      end
    end

    def with_notification_exclusion(owners, &block)
      return yield if owners.empty?
      id = owners.first
      result = @store.lock("notifications-#{id}", nonblock: true) do
        @notification_locks << id
        with_notification_exclusion(owners.drop(1), &block)
      end
      result || with_notification_exclusion(owners.drop(1), &block)
    end

    def with_integration_exclusion(paths, apply:, &block)
      return yield if paths.empty?
      id = File.basename(paths.first, '.json')
      raise Error, 'unknown integration journal; collection refused' unless id.match?(UUID)
      # Drivers already hold this lock over preparation and publication, including
      # legacy versions. Never wait here while holding the profile exclusion.
      result = @store.lock("integration-#{id}", nonblock: true) do
        with_integration_exclusion(paths.drop(1), apply: apply, &block)
      end
      result || {'schema' => 1, 'mode' => apply ? 'apply' : 'dry-run', 'actions' => [], 'removed' => [],
        'preserved' => [{'path' => paths.first, 'reason' => 'integration driver is running'}], 'retry' => true}
    end

    def sweep(apply: false)
      @store.lock('retention') do
        @process_references = nil
        @ledger = @store.read
        @book = @retention.read
        @metadata, @snapshots = {}, {}
        @payloads, @live_payloads, @live_ids, @live_paths = {}, Set.new, Set.new, Set.new
        @journals = @store.integration_journals.to_h { |run| [run['id'], run] }
        @skipped, @actions = [], []
        preparing = preparation_processes
        unless preparing.empty?
          return {'schema' => 1, 'mode' => apply ? 'apply' : 'dry-run', 'actions' => [], 'removed' => [],
            'preserved' => preparing.map { |pid| {'pid' => pid, 'reason' => 'preparation is running'} }, 'retry' => true}
        end
        inventory
        inventory_integrations
        recover_pending
        recover_integration_pending
        adopt_preparations
        mark_live
        plan
        @retention.write(@book) if apply
        removed = []
        @actions.each do |action|
          break if @store.read['revision'] != @ledger['revision']
          next unless apply
          if action['kind'] == 'worktree'
            remove_worktree(action)
          elsif action['kind'] == 'profile_generation'
            remove_profile_generation(action)
          else
            File.unlink(action.fetch('path')) if File.symlink?(action.fetch('path'))
          end
          removed << action
        end
        {'schema' => 1, 'mode' => apply ? 'apply' : 'dry-run', 'actions' => @actions,
         'removed' => removed, 'preserved' => @skipped,
         'retry' => @store.read['revision'] != @ledger['revision']}
      end
    end

    def preparation_processes
      return [] if @engine.instance_variable_get(:@fixtures)
      Dir['/proc/[0-9]*'].filter_map do |directory|
        next if File.basename(directory).to_i == Process.pid || File.stat(directory).uid != Process.uid
        argv = File.binread(File.join(directory, 'cmdline')).split("\0")
        native = argv.first(2).any? { |arg| File.basename(arg).match?(/\A\.?(?:workspace-host|dev-session)(?:-wrapped)?\z/) }
        next unless native && (argv.include?('prepare') || argv.include?('coordination-bootstrap'))
        File.basename(directory).to_i
      rescue Errno::ENOENT, Errno::ESRCH
        nil
      rescue Errno::EACCES
        raise Error, 'cannot inspect preparation processes; collection refused'
      end
    end

    def payload_in_use?(path)
      @process_references ||= Dir['/proc/[0-9]*'].flat_map do |directory|
        next [] if File.stat(directory).uid != Process.uid
        links = [File.join(directory, 'cwd')] + Dir[File.join(directory, 'fd', '*')]
        links.filter_map do |link|
          File.readlink(link).delete_suffix(' (deleted)')
        rescue Errno::ENOENT, Errno::ESRCH, Errno::EINVAL
          nil
        rescue Errno::EACCES
          # Nondumpable, unrelated user services need not expose descriptors.
          # Durable claims and admission are authoritative; command-line paths
          # additionally cover descendants of an exited legacy preparation.
          File.binread(File.join(directory, 'cmdline')).split("\0")
        end.flatten
      rescue Errno::ENOENT, Errno::ESRCH, Errno::EACCES
        []
      end
      prefix = path + '/'
      @process_references.any? { |target| target == path || target.start_with?(prefix) }
    end

    def inventory
      Dir[File.join(@sources, '*')].sort.each do |directory|
        raise Error, 'coordination source directory is a symlink' if File.symlink?(directory)
        next unless File.directory?(directory)
        name = File.basename(directory)
        %w[artifact snapshot].each do |kind|
          path = File.join(directory, "#{kind}.json")
          next unless File.file?(path)
          value = WorkspaceCoordination.private_read(path, limit: MAX_REQUEST)
          sealed = kind == 'artifact' ? value : value.fetch('snapshot')
          unless name.match?(DIGEST) && sealed['id'] == "sha256:#{name}" &&
                 sealed['id'] == WorkspaceCoordination.digest(sealed.reject { |key, _| key == 'id' })
            raise Error, 'coordination metadata identity changed; collection refused'
          end
          @metadata[sealed['id']] = value
          @snapshots[sealed['id']] = value if kind == 'snapshot'
        end
        { 'checkout' => SOURCE, 'target' => UUID }.each do |leaf, format|
          path = File.join(directory, leaf)
          next unless File.exist?(path) || File.symlink?(path)
          unless name.match?(format) && File.directory?(path) && !File.symlink?(path)
            @skipped << {'path' => path, 'reason' => 'unknown payload layout'}
            next
          end
          @payloads[path] = {'kind' => 'worktree', 'path' => path, 'source' => name}
        end
      end
    rescue KeyError => error
      raise Error, "invalid coordination metadata: #{error.message}"
    end

    def recover_pending
      @book['pending'].each do |relative, action|
        expected = File.join(@sources, relative)
        unless relative.match?(%r{\A(?:[0-9a-f]{40}/checkout|[0-9a-f-]{36}/target)\z}) &&
               action.is_a?(Hash) && action['path'] == expected && action['common'].is_a?(String) &&
               action['common'].start_with?('/') && action['head'].to_s.match?(SOURCE)
          raise Error, 'invalid interrupted collection record'
        end
        @payloads[expected] ||= action.merge('missing' => !File.exist?(expected))
      end
    end

    def integration_finished?(run)
      run['phase'] == 'complete' && (run['schema'] == 1 || run.dig('continuation', 'state') == 'complete')
    end

    def inventory_integrations
      Dir[File.join(@store.root, 'integrations', '*')].sort.each do |directory|
        next unless File.directory?(directory) || File.symlink?(directory)
        id = File.basename(directory)
        run = @journals[id]
        unless id.match?(UUID) && run && !File.symlink?(directory) && integration_finished?(run)
          @skipped << {'path' => directory, 'reason' => 'unknown or unfinished integration'}
          next
        end
        repositories = Array(run.dig('request', 'repositories')).filter_map { |item| item['repository'] if item.is_a?(Hash) }
        Dir[File.join(directory, '*')].sort.each do |path|
          match = File.basename(path).match(/\A(.+)-[0-9a-f]{12}-[0-9a-f]{12}\z/)
          unless match && repositories.include?(match[1]) && File.directory?(path) && !File.symlink?(path)
            @skipped << {'path' => path, 'reason' => 'unknown integration payload layout'}
            next
          end
          @payloads[path] = {'kind' => 'worktree', 'path' => path, 'source' => id,
            'integration' => id, 'repository' => match[1]}
        end
      end
    end

    def recover_integration_pending
      @journals.each_value do |run|
        pending = run.fetch('payloadGcPending', {})
        raise Error, 'invalid integration collection record' unless pending.is_a?(Hash)
        pending.each do |leaf, action|
          path = File.join(@store.root, 'integrations', run['id'], leaf)
          unless leaf.match?(/\A[A-Za-z0-9][A-Za-z0-9_-]*-[0-9a-f]{12}-[0-9a-f]{12}\z/) &&
                 action.is_a?(Hash) && action['path'] == path && action['integration'] == run['id'] &&
                 action['kind'] == 'worktree' && action['source'] == run['id'] &&
                 action['common'].is_a?(String) && action['head'].to_s.match?(SOURCE) &&
                 action['repository'].is_a?(String) && File.basename(path).start_with?(action['repository'] + '-') &&
                 Array(run.dig('request', 'repositories')).any? { |item| item.is_a?(Hash) && item['repository'] == action['repository'] }
            raise Error, 'invalid interrupted integration collection record'
          end
          if File.symlink?(path) || File.exist?(path) && !File.directory?(path)
            raise Error, 'interrupted integration checkout is not an ordinary directory'
          end
          @payloads[path] = action.merge('missing' => !File.exist?(path))
          @live_payloads << path unless integration_finished?(run)
        end
      end
    end

    def verify_integration_repository(action, common)
      run = @journals.fetch(action.fetch('integration'))
      owner = @ledger['owners'][run['ownerId']]
      raise Error, 'integration checkout owner is unavailable' unless owner
      common = File.realpath(common)
      root = owner.fetch('workspace')
      unless common == File.join(root, '.git') || common.start_with?(File.join(root, 'repos') + '/')
        raise Error, 'integration checkout belongs to another workspace'
      end
      remote = @engine.capture('git', '--git-dir', common, 'remote', 'get-url', 'origin').strip
      expected = @engine.catalog.repository(action.fetch('repository')).fetch('remote')
      fixtures = @engine.instance_variable_get(:@fixtures)
      unless WorkspaceCoordination.remote_identity(remote, fixtures: fixtures) == WorkspaceCoordination.remote_identity(expected, fixtures: fixtures)
        raise Error, 'integration checkout repository changed'
      end
    end

    def protect_payload(action)
      @metadata.each_value do |value|
        bytes = JSON.generate(value)
        mark(value) if bytes.include?(action['path']) || bytes.include?(action['source'])
      end
    end

    def referenced_snapshot?(id)
      @ledger['jobs'].values.any? do |job|
        ([job['snapshot']] + job['history'].filter_map { |item| item['snapshot'] }).any? { |item| item['id'] == id }
      end
    end

    def adopt_preparations
      @snapshots.each_key do |id|
        next if @book['preparations'].key?(id) || referenced_snapshot?(id)
        @book['preparations'][id] = {'expiresAt' => Time.now.to_i + Retention::TTL,
          'bindings' => @retention.bindings(id, @ledger)}
      end
    end

    def mark(value)
      case value
      when Hash then value.each_value { |item| mark(item) }
      when Array then value.each { |item| mark(item) }
      when String
        @live_paths << value
        value.scan(/sha256:[0-9a-f]{64}/).each do |id|
          next if @live_ids.include?(id)
          @live_ids << id
          mark(@metadata[id]) if @metadata.key?(id)
        end
        @payloads.each_key { |path| @live_payloads << path if value.include?(path) }
        if value.match?(SOURCE)
          path = File.join(@sources, value, 'checkout')
          @live_payloads << path if @payloads.key?(path)
        end
      end
    end

    def attempt_running?(job)
      attempt = job['attempt']
      return false unless attempt
      directory = File.join(@store.root, 'attempts', attempt)
      lock = File.join(directory, 'worker.lock')
      if File.file?(lock)
        File.open(lock, File::RDONLY) do |file|
          return true unless file.flock(File::LOCK_EX | File::LOCK_NB)
        end
      end
      !@engine.instance_variable_get(:@fixtures) && @engine.attempt_service_running?(attempt)
    end

    def mark_live
      @live_attempts = Set.new
      @known_attempts = Set.new
      @ledger['jobs'].each_value do |job|
        @known_attempts << job['attempt'] if job['attempt']
        job['history'].each { |item| @known_attempts << item.dig('result', 'attemptId') if item.dig('result', 'attemptId') }
        owner = @ledger['owners'].fetch(job['ownerId'])
        live = !TERMINAL.include?(job['state']) || attempt_running?(job) || owner['fence'] && !owner['fence']['completed']
        next unless live
        mark(job)
        @live_attempts << job['attempt'] if job['attempt']
      end
      @ledger['reservations'].each_value { |item| mark(item) unless item['state'] == 'released' }
      @ledger['outbox'].each_value { |item| mark(item) unless item['state'] == 'delivered' }
      @snapshots.each do |id, value|
        owner = @ledger['owners'][value['ownerId']]
        closed = owner && owner['fence'] && owner['fence']['completed']
        mark(value) if !closed && @retention.live?(id, @ledger, @book)
      end
      @journals.each_value do |value|
        next if integration_finished?(value)
        mark(value)
        # Existing journals bind source heads, but do not record hook artifact
        # IDs. Derive those dependencies from the sealed merge-hook artifacts.
        journal = JSON.generate(value)
        @metadata.each_value do |artifact|
          head = artifact['featureHead']
          mark(artifact) if artifact['kind'] == 'merge_hooks' && head.is_a?(String) && head.match?(SOURCE) && journal.include?(head)
        end
      end
    end

    def plan
      all_metadata = JSON.generate(@metadata)
      @payloads.each_value do |action|
        path = action['path']
        key = path.delete_prefix(@sources + '/')
        unless action['integration'] || all_metadata.include?(path) || all_metadata.include?(action['source']) || @book['pending'].key?(key)
          @book['orphans'][key] ||= Time.now.to_i + Retention::TTL
          unless @book['orphans'][key].is_a?(Integer)
            raise Error, 'invalid orphan retention age'
          end
          if @book['orphans'][key] > Time.now.to_i
            @skipped << {'path' => path, 'reason' => 'unsubmitted payload adoption grace'}
            next
          end
        end
        if File.exist?(File.join(path, '.keep')) || File.symlink?(File.join(path, '.keep'))
          protect_payload(action)
          @skipped << {'path' => path, 'reason' => 'explicit hold'}
          next
        end
        if payload_in_use?(path)
          protect_payload(action)
          @skipped << {'path' => path, 'reason' => 'source is in use by a process'}
          next
        end
        if @live_payloads.include?(path)
          @skipped << {'path' => path, 'reason' => 'required by live work or preparation'}
          next
        end
        begin
          if action['integration'] && action['head']
            # Admission already proved this disposable checkout clean. Partial
            # removal may have deleted tracked files or .git, so use Git's
            # retained registration rather than readmission as a clean checkout.
            common = action.fetch('common')
            verify_integration_repository(action, common)
            registered = @engine.capture('git', '--git-dir', common, 'worktree', 'list', '--porcelain')
            raise Error, 'retained worktree is locked' if worktree_locked?(registered, path)
            record = registered.split("\n\n").find { |item| item.lines.include?("worktree #{path}\n") }
            if record
              unless record.lines.include?("HEAD #{action['head']}\n") && record.lines.map(&:strip).include?('detached')
                raise Error, 'interrupted integration checkout registration changed'
              end
            elsif !action['missing']
              raise Error, 'interrupted integration checkout registration is missing'
            end
            if File.exist?(File.join(path, '.git')) || File.symlink?(File.join(path, '.git'))
              actual = @engine.capture('git', '-C', path, 'rev-parse', '--path-format=absolute', '--git-common-dir').strip
              raise Error, 'interrupted integration checkout repository changed' unless File.realpath(actual) == File.realpath(common)
              head = @engine.capture('git', '-C', path, 'rev-parse', 'HEAD').strip
              raise Error, 'interrupted integration checkout HEAD changed' unless head == action['head']
            end
            if git_resource_busy?(common)
              raise Error, 'Git resource is busy'
            end
            @actions << action
            next
          end
          if action['missing']
            @actions << action
            next
          end
          common = @engine.capture('git', '-C', path, 'rev-parse', '--path-format=absolute', '--git-common-dir').strip
          verify_integration_repository(action, common) if action['integration']
          if git_resource_busy?(common)
            protect_payload(action)
            @skipped << {'path' => path, 'reason' => 'Git resource is busy'}
            next
          end
          head = @engine.capture('git', '-C', path, 'rev-parse', 'HEAD').strip
          unless action['source'].match?(UUID) || head == action['source']
            raise Error, 'retained source HEAD changed'
          end
          dirty = @engine.capture('git', '-C', path, 'status', '--porcelain', '--untracked-files=all').strip
          raise Error, 'retained worktree has local changes' unless dirty.empty?
          registered = @engine.capture('git', '--git-dir', common, 'worktree', 'list', '--porcelain')
          raise Error, 'retained worktree is locked' if worktree_locked?(registered, path)
          raise Error, 'payload is not a registered worktree' unless registered.lines.include?("worktree #{path}\n")
          if action['integration'] && !registered.split("\n\n").any? { |record| record.lines.include?("worktree #{path}\n") && record.lines.map(&:strip).include?('detached') }
            raise Error, 'integration checkout is not detached'
          end
          @actions << action.merge('common' => common, 'head' => head)
        rescue Error => error
          protect_payload(action)
          @skipped << {'path' => path, 'reason' => error.message}
        end
      end
      @actions.reject! { |action| @live_payloads.include?(action['path']) }
      Dir[File.join(@store.root, 'roots', '*')].sort.each do |path|
        next unless File.symlink?(path)
        name = File.basename(path)
        worker = name.match(/\A(#{UUID.source.delete_prefix('\\A').delete_suffix('\\z')})-([0-9a-f]{12})-(notification|integration)\z/)
        if worker
          plan_worker_root(path, worker)
          next
        end
        id = name[/\A([0-9a-f]{64})-/, 1]
        attempt = name[/\A(#{UUID.source.delete_prefix('\\A').delete_suffix('\\z')})-/, 1]
        known = id && @metadata.key?("sha256:#{id}") || attempt && @known_attempts.include?(attempt)
        live = id && @live_ids.include?("sha256:#{id}") || attempt && @live_attempts.include?(attempt)
        if known && !live
          @actions << {'kind' => 'root', 'path' => path}
        else
          @skipped << {'path' => path, 'reason' => live ? 'required by live work' : 'unknown root ownership'}
        end
      end
      plan_profile_generations
    end

    def service_running?(unit)
      return false if @engine.instance_variable_get(:@fixtures)
      properties = @engine.capture('systemctl', '--user', 'show', unit,
        '--property=ActiveState,MainPID').lines.to_h { |line| line.strip.split('=', 2) }
      properties['MainPID'].to_i > 0 || %w[active activating deactivating].include?(properties['ActiveState'])
    end

    def plan_worker_root(path, match)
      id, digest, kind = match.captures
      known = kind == 'notification' ? @ledger['owners'].key?(id) : @journals.key?(id)
      unless known && Digest::SHA256.hexdigest(File.realpath(path))[0, 12] == digest
        @skipped << {'path' => path, 'reason' => 'unknown worker root ownership'}
        return
      end
      live = if kind == 'notification'
        !@notification_locks.include?(id) || service_running?("workspace-notifications-#{id}.service")
      else
        !integration_finished?(@journals.fetch(id)) || service_running?("workspace-integration-#{id}.service")
      end
      if live
        @skipped << {'path' => path, 'reason' => 'worker is running or integration is unfinished'}
      else
        @actions << {'kind' => 'root', 'path' => path}
      end
    rescue Errno::ENOENT, Errno::ELOOP
      @skipped << {'path' => path, 'reason' => 'worker root target is unavailable'}
    end

    def plan_profile_generations
      profile = @engine.profile
      return unless File.symlink?(profile)
      current_path = File.expand_path(File.readlink(profile), File.dirname(profile))
      match = current_path.match(/\A#{Regexp.escape(profile)}-([0-9]+)-link\z/)
      return unless match && File.symlink?(current_path)
      current = match[1].to_i
      generations = Dir["#{profile}-*-link"].filter_map do |path|
        item = path.match(/\A#{Regexp.escape(profile)}-([0-9]+)-link\z/)
        [item[1].to_i, path] if item && File.symlink?(path)
      end.sort
      keep = generations.select { |number, _| number <= current }.last(2).map(&:first)
      generations.each do |number, path|
        next if number > current || keep.include?(number)
        target = File.realpath(path)
        if @live_paths.any? { |value| value == target || value.start_with?(target + '/') }
          @skipped << {'path' => path, 'reason' => 'profile generation is required by live work'}
          next
        end
        @actions << {'kind' => 'profile_generation', 'path' => path, 'profile' => profile,
          'generation' => number, 'profileToken' => DevWorkspaceProfileIdentity.token(profile), 'target' => target}
      end
    end

    def remove_profile_generation(action)
      unless DevWorkspaceProfileIdentity.token(action['profile']) == action['profileToken'] &&
             File.symlink?(action['path']) && File.realpath(action['path']) == action['target']
        raise Error, 'workspace profile changed during collection; retry'
      end
      @engine.capture('nix-env', '--profile', action['profile'], '--delete-generations', action['generation'].to_s)
    end

    def worktree_locked?(registered, path)
      registered.split("\n\n").any? do |record|
        record.lines.include?("worktree #{path}\n") &&
          record.lines.any? { |line| line.strip == 'locked' || line.start_with?('locked ') }
      end
    end

    def git_resource_busy?(common)
      resource = "git:#{File.realpath(common)}"
      @store.held_claim_resources.include?(resource) ||
        @ledger['jobs'].values.any? { |job| CLAIMED.include?(job['state']) && job['resources'].include?(resource) }
    end

    def remove_worktree(action)
      raise Error, 'Git resource is busy' if git_resource_busy?(action.fetch('common'))
      path = action.fetch('path')
      key = path.delete_prefix(@sources + '/')
      if action['integration']
        run = @journals.fetch(action['integration'])
        run['payloadGcPending'] ||= {}
        run['payloadGcPending'][File.basename(path)] = action
        save_integration_gc(run)
      else
        @book['pending'][key] = action
        @retention.write(@book)
      end
      # Ignored shell dependencies are owned preparation output. The explicit
      # porcelain check above still protects all tracked and untracked edits.
      registered = @engine.capture('git', '--git-dir', action.fetch('common'), 'worktree', 'list', '--porcelain')
      raise Error, 'retained worktree is locked' if worktree_locked?(registered, path)
      if File.exist?(path) || registered.lines.include?("worktree #{path}\n")
        if action['integration'] && File.directory?(path) && !File.exist?(File.join(path, '.git'))
          # Git refuses a surviving directory after .git was deleted. Finish
          # this already-admitted removal, then let Git retire its registration.
          FileUtils.remove_entry(path)
        end
        @engine.capture('git', '--git-dir', action.fetch('common'), 'worktree', 'remove', '--force', path)
      end
      if action['integration']
        run['payloadGcPending'].delete(File.basename(path))
        run.delete('payloadGcPending') if run['payloadGcPending'].empty?
        save_integration_gc(run)
      else
        @book['pending'].delete(key)
        @retention.write(@book)
      end
    end

    def save_integration_gc(run)
      WorkspaceCoordination.atomic(File.join(@store.root, 'integrations', "#{run.fetch('id')}.json"), run)
    end
  end
end
