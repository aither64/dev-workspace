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

  # Caller holds the existing exclusive profile-transition guard. Old and new
  # preparation commands already hold its shared side. Workers may finish, but
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
      with_integration_exclusion(Dir[File.join(@store.root, 'integrations', '*.json')].sort, apply: apply) do
        sweep(apply: apply)
      end
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
        @payloads, @live_payloads, @live_ids = {}, Set.new, Set.new
        @skipped, @actions = [], []
        preparing = preparation_processes
        unless preparing.empty?
          return {'schema' => 1, 'mode' => apply ? 'apply' : 'dry-run', 'actions' => [], 'removed' => [],
            'preserved' => preparing.map { |pid| {'pid' => pid, 'reason' => 'preparation is running'} }, 'retry' => true}
        end
        inventory
        recover_pending
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
      Dir[File.join(@store.root, 'integrations', '*.json')].each do |path|
        value = WorkspaceCoordination.private_read(path)
        raise Error, 'unknown integration journal; collection refused' unless value['schema'] == 1 && value['phase'].is_a?(String)
        next if value['phase'] == 'complete'
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
        unless all_metadata.include?(path) || all_metadata.include?(action['source']) || @book['pending'].key?(key)
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
          if action['missing']
            @actions << action
            next
          end
          common = @engine.capture('git', '-C', path, 'rev-parse', '--path-format=absolute', '--git-common-dir').strip
          if @ledger['jobs'].values.any? { |job| CLAIMED.include?(job['state']) && job['resources'].include?("git:#{common}") }
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
          raise Error, 'payload is not a registered worktree' unless registered.lines.include?("worktree #{path}\n")
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
    end

    def remove_worktree(action)
      path = action.fetch('path')
      key = path.delete_prefix(@sources + '/')
      @book['pending'][key] = action
      @retention.write(@book)
      # Ignored shell dependencies are owned preparation output. The explicit
      # porcelain check above still protects all tracked and untracked edits.
      registered = @engine.capture('git', '--git-dir', action.fetch('common'), 'worktree', 'list', '--porcelain')
      if File.exist?(path) || registered.lines.include?("worktree #{path}\n")
        @engine.capture('git', '--git-dir', action.fetch('common'), 'worktree', 'remove', '--force', path)
      end
      @book['pending'].delete(key)
      @retention.write(@book)
    end
  end
end
