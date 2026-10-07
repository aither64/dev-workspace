# frozen_string_literal: true

module DevSession
  # Archive owns this inventory. Delete and individual worktree commands retain
  # their older, deliberately narrower discovery and removal rules.
  class ArchiveCleanup
    # Explicit collaborator implemented by Runner. The inventory owner uses
    # only this interface plus workspace; it does not inspect Runner state or
    # dispatch arbitrary private methods. Tracking and runtime remain Runner's
    # authority, and registered branch proofs retain the existing fetch policy.
    module RunnerContext
      def archive_cleanup_capture(argv, **options)
        @command_runner.capture(argv, **options)
      end

      def archive_cleanup_remove!(common_dir, path)
        @command_runner.run(['git', "--git-dir=#{common_dir}", 'worktree', 'remove', path])
      end

      def archive_cleanup_repository(project)
        repository_common_dir(project)
      end

      def archive_cleanup_github_repository(origin)
        github_repository(origin)
      end

      def archive_cleanup_check_registration!(repository, common, path)
        validate_portal_repository_identity!(repository, common, path)
      end

      def archive_cleanup_branch_proof(repository, common, branch, default, allow_unpushed:)
        archive_branch_proof(repository, common, branch, default, allow_unpushed:)
      end

      def archive_cleanup_shared_master_retained?(repository, common, head, mode:)
        return false unless repository['project'] == 'workspace' &&
                            repository['branch'] == 'master' && repository['default_branch'] == 'master' &&
                            common == File.realpath(File.join(workspace, '.git'))

        # Archival itself and other sessions append tracking commits to master.
        refs = ['refs/heads/master']
        if mode == 'complete'
          fetch_archive_refs!(common, ['master'])
          refs << 'refs/remotes/origin/master'
        end
        refs.each do |ref|
          tip = resolve_commit(common, ref)
          _stdout, _stderr, status = @command_runner.capture(
            ['git', "--git-dir=#{common}", 'merge-base', '--is-ancestor', head, tip], allow_failure: true
          )
          unless status.success?
            raise Error, "shared workspace #{ref} no longer retains sealed HEAD #{head}"
          end
        end
        true
      end

      def archive_cleanup_manifest(slug)
        tracking = path_exists?(work_dir(slug)) ? work_dir(slug) : archive_dir(slug)
        load_portal_manifest(File.join(tracking, PORTAL_MANIFEST), required: false)
      end

      def archive_cleanup_session(slug)
        session = cleanup_session(slug)
        session ? ensure_managed_session!(slug, session:) : nil
      end

      def archive_cleanup_source_digest(slug)
        tracking_tree_sha256(work_dir(slug))
      end

      def archive_cleanup_source_matches?(slug, digest)
        tracking_tree_matches?(work_dir(slug), digest)
      end

      def archive_cleanup_read_sidecar(path)
        read_private_json_object(path, 'archive cleanup sidecar', max_size: ArchiveCleanup::MAX_BYTES, mode: 0o600)
      end

      def archive_cleanup_prepare_storage!
        lifecycle_journal_root
      end

      def archive_cleanup_check_tracking!(slug, journal, plan, heads)
        if path_exists?(work_dir(slug))
          verify_pending_archive_tracking!(slug, journal, plan, final_heads: heads)
        else
          verify_archived_tracking_tree!(slug, journal)
        end
      end

      def archive_cleanup_check_completed!(slug, journal, recorded_retirement:)
        verify_committed_archive_tracking!(slug, journal)
        require_completed_archive_retirement!(slug, journal, recorded_retirement:)
      end
    end

    MAX_BYTES = 1024 * 1024
    SHA = /\A(?:[0-9a-f]{40}|[0-9a-f]{64})\z/
    DIGEST = /\A[0-9a-f]{64}\z/
    BINDING_KEYS = %w[workspace slug operation_id mode finalized_at target_tracking_sha256 retained_thread_id].freeze
    KEYS = (BINDING_KEYS + %w[schema sealed inventory_sha256 inventory completed]).freeze
    INVENTORY_KEYS = %w[worktrees containers proofs registered_heads tracking_identity source_sha256].freeze
    WORKTREE_KEYS = %w[path project common_dir common_identity admin_dir admin_identity identity head branch dirty removed].freeze
    PROOF_KEYS = %w[kind name project common_dir common_identity origin branch default_branch head ref tip initial_base_sha allow_unpushed].freeze

    # Structural input boundary shared with the reviewed migration plan. Actual
    # registration, checkout and ref proof remains discover/revalidate here.
    def self.discovery_shape?(inventory)
      identity = ->(value) { value.is_a?(Hash) && value.keys.sort == %w[dev ino] && value.values.all? { |part| part.is_a?(Integer) && part >= 0 } }
      relative = ->(value, empty = false) { value.is_a?(String) && (empty && value.empty? || !value.empty? && !value.start_with?('/') && !value.match?(/[[:cntrl:]]/) && value.split('/').none? { |part| part.empty? || %w[. .. .git].include?(part) }) }
      inventory.is_a?(Hash) && inventory.keys.sort == %w[containers worktrees] &&
        %w[worktrees containers].all? { |key| inventory[key].is_a?(Array) } &&
        inventory['worktrees'].all? { |record| record.is_a?(Hash) && record.keys.sort == WORKTREE_KEYS.sort &&
          relative.call(record['path']) && record['project'].is_a?(String) && record['project'].match?(/\A[A-Za-z0-9][A-Za-z0-9_-]*\z/) &&
          %w[common_dir admin_dir].all? { |key| record[key].is_a?(String) && record[key].start_with?('/') && !record[key].match?(/[[:cntrl:]]/) } &&
          %w[identity common_identity admin_identity].all? { |key| identity.call(record[key]) } && record['head'].is_a?(String) && record['head'].match?(SHA) &&
          (record['branch'].nil? || record['branch'].is_a?(String) && !record['branch'].empty? && !record['branch'].match?(/[[:cntrl:]]/)) &&
          record['dirty'] == false && [true, false].include?(record['removed']) } &&
        inventory['containers'].all? { |record| record.is_a?(Hash) && record.keys.sort == %w[identity path removed] && relative.call(record['path'], true) && identity.call(record['identity']) && [true, false].include?(record['removed']) } &&
        %w[worktrees containers].all? { |key| inventory[key].map { |record| record['path'] }.uniq.length == inventory[key].length }
    end

    def initialize(runner, slug)
      @runner = runner
      @slug = slug
      @workspace = runner.workspace
      @group = File.join(@workspace, 'worktrees', slug)
    end

    def path
      File.join(@workspace, 'worktrees', '.locks', "#{@slug}.archive-cleanup.json")
    end

    def load(journal: nil, operation_id: nil)
      return unless exists?(path)

      raise Error, "archive cleanup sidecar is a symlink: #{path}" if File.symlink?(path)
      # StrictJSONHash rejects duplicates while parsing persisted input. Work
      # with ordinary hashes afterward so progress can replace existing keys.
      sidecar = canonical(@runner.archive_cleanup_read_sidecar(path))
      validate!(sidecar)
      if operation_id && sidecar.fetch('operation_id') != operation_id
        raise Error, "session archive cleanup operation changed: #{@slug}"
      end
      match_journal!(sidecar, journal) if journal
      sidecar
    end

    def discover(clean: true)
      reject_symlink_components!(@group)
      directory_identity(@group) if exists?(@group)
      candidates = registrations.select { |record| below_group?(record.fetch('path')) }
      paths = candidates.map { |record| record.fetch('path') }
      if paths.uniq.length != paths.length || paths.any? { |a| paths.any? { |b| a != b && a.start_with?(b + '/') } }
        raise Error, "duplicate or overlapping archive worktrees: #{@slug}"
      end
      worktrees = candidates.sort_by { |record| record.fetch('path') }.map do |record|
        inspect_checkout(record, clean:)
      end
      containers = []
      walk_containers(@group, worktrees.map { |record| absolute(record.fetch('path')) }, containers) if exists?(@group)
      { 'worktrees' => worktrees, 'containers' => containers.sort_by { |record| record.fetch('path') } }
    end

    # Read-only canonical discovery shared with legacy normalization. Aliases
    # are deduplicated by the same common-directory owner as cleanup.
    def repository_catalog
      canonical_repositories.map { |project, common| { 'project' => project, 'common_dir' => common } }
    end

    def prove(inventory, mode)
      manifest = current_manifest
      repositories = manifest&.fetch('repositories', []) || []
      if !manifest && mode == 'complete' && !inventory.fetch('worktrees').empty?
        raise Error, 'normalize legacy repository registrations before complete archival'
      end
      registered_heads = {}
      failures = []
      proofs = repositories.map do |repository|
        common = @runner.archive_cleanup_repository(repository.fetch('project'))
        checkout = inventory.fetch('worktrees').find { |record| record.fetch('path') == repository.fetch('name') }
        if checkout
          unless checkout.fetch('common_dir') == common && checkout.fetch('branch') == repository.fetch('branch')
            raise Error, "registered archive checkout changed: #{repository.fetch('name')}"
          end
          @runner.archive_cleanup_check_registration!(repository, common, absolute(checkout.fetch('path')))
        end
        proof = make_proof('registered', repository.fetch('name'), repository, common, mode,
                           allow_unpushed: !checkout.nil?)
        if checkout && checkout.fetch('head') != proof.fetch('head')
          raise Error, "registered checkout HEAD differs from branch: #{repository.fetch('name')}"
        end
        registered_heads[repository.fetch('name')] = proof.fetch('head')
        proof
      rescue Error, CommandError => e
        raise unless mode == 'complete'

        failures << "#{repository.fetch('project')}/#{repository.fetch('name')}: #{e.message.lines.first.to_s.strip}"
        nil
      end.compact
      inventory.fetch('worktrees').each do |checkout|
        next if repositories.any? { |repository| repository.fetch('name') == checkout.fetch('path') }

        common = checkout.fetch('common_dir')
        default = default_branch(common, repositories, required: mode == 'complete')
        branch = checkout.fetch('branch')
        kind = branch.nil? ? 'detached' : branch == default ? 'default' : 'additional'
        repository = { 'project' => checkout.fetch('project'), 'branch' => branch, 'default_branch' => default }
        proof = make_proof(kind, checkout.fetch('path'), repository, common, mode,
                           head: checkout.fetch('head'), allow_unpushed: false)
        proofs << proof
      rescue Error, CommandError => e
        raise unless mode == 'complete'

        failures << "#{checkout.fetch('project')}/#{checkout.fetch('path')}: #{e.message.lines.first.to_s.strip}"
      end
      raise Error, "initiative cannot be completed:\n- #{failures.join("\n- ")}" unless failures.empty?

      inventory.merge('proofs' => proofs, 'registered_heads' => registered_heads)
    end

    def plan(inventory)
      manifest_names = (current_manifest&.fetch('repositories', []) || []).map { |repository| repository.fetch('name') }
      entries = inventory.fetch('worktrees').filter_map do |record|
        next if record.fetch('removed') || !manifest_names.include?(record.fetch('path'))

        { name: record.fetch('path'), path: absolute(record.fetch('path')), common_dir: record.fetch('common_dir') }
      end
      session = @runner.archive_cleanup_session(@slug)
      Runner::CleanupPlan.new(session:, entries:, unexpected: [])
    end

    def additional_features?(inventory, repositories)
      inventory.fetch('worktrees').any? do |record|
        branch = record.fetch('branch')
        branch && branch != default_branch(record.fetch('common_dir'), repositories, required: false)
      end
    end

    def create(journal, inventory)
      source = source_path
      inventory = inventory.merge('tracking_identity' => directory_identity(source),
                                  'source_sha256' => @runner.archive_cleanup_source_digest(@slug))
      sidecar = journal.slice(*BINDING_KEYS).merge(
        'schema' => 1, 'sealed' => true, 'inventory' => inventory,
        'inventory_sha256' => inventory_digest(inventory), 'completed' => false
      )
      @runner.archive_cleanup_prepare_storage!
      write(sidecar, create: true)
      sidecar
    end

    def match_journal!(sidecar, journal)
      heads = sidecar.fetch('mode') == 'complete' ? sidecar.fetch('inventory').fetch('registered_heads') : {}
      unless BINDING_KEYS.all? { |key| sidecar.fetch(key) == journal.fetch(key) } &&
             journal.fetch('proven_heads') == heads
        raise Error, "archive journal and cleanup sidecar disagree: #{@slug}"
      end
    end

    def prepared_journal(sidecar)
      raise Error, "completed archive cleanup cannot start another archive: #{@slug}" if sidecar.fetch('completed')

      inventory = sidecar.fetch('inventory')
      source = source_path
      unless !exists?(archive_path) && directory_identity(source) == inventory.fetch('tracking_identity') &&
             @runner.archive_cleanup_source_matches?(@slug, inventory.fetch('source_sha256')) &&
             inventory.fetch('worktrees').none? { |record| record.fetch('removed') } &&
             inventory.fetch('containers').none? { |record| record.fetch('removed') }
        raise Error, "prepared archive cleanup source changed: #{@slug}"
      end
      current = discover
      unless %w[worktrees containers].all? { |kind| current.fetch(kind) == inventory.fetch(kind) }
        raise Error, "prepared archive cleanup inventory changed: #{@slug}"
      end
      sidecar.slice(*BINDING_KEYS).merge(
        'schema' => 2, 'phase' => 'prepared',
        'proven_heads' => sidecar.fetch('mode') == 'complete' ? inventory.fetch('registered_heads') : {}
      )
    end

    def revalidate!(sidecar, journal: nil)
      validate!(sidecar)
      match_journal!(sidecar, journal) if journal
      inventory = sidecar.fetch('inventory')
      current = discover
      %w[worktrees containers].each do |kind|
        expected = inventory.fetch(kind).to_h { |record| [record.fetch('path'), record] }
        actual = current.fetch(kind).to_h { |record| [record.fetch('path'), record] }
        additions = actual.keys - expected.keys
        raise Error, "archive cleanup entries were added: #{additions.join(', ')}" unless additions.empty?

        expected.each do |relative, record|
          if actual[relative]
            if record.fetch('removed') || actual.fetch(relative) != record.merge('removed' => false)
              raise Error, "archive cleanup identity changed or removed path reappeared: #{absolute(relative)}"
            end
          else
            if kind == 'worktrees'
              prove_absent!(record)
            elsif exists?(absolute(relative))
              raise Error, "archive cleanup container changed: #{absolute(relative)}"
            end
            record['removed'] = true
          end
        end
      end
      inventory.fetch('proofs').each { |proof| reprove!(proof, sidecar.fetch('mode')) }
      tracking = exists?(source_path) ? source_path : archive_path
      unless directory_identity(tracking) == inventory.fetch('tracking_identity')
        raise Error, "archive tracking directory identity changed: #{@slug}"
      end
      @runner.archive_cleanup_check_tracking!(@slug, journal, plan(inventory), inventory.fetch('registered_heads')) if journal
      write(sidecar)
      sidecar
    end

    def remove!(sidecar, journal)
      revalidate!(sidecar, journal:)
      sidecar.fetch('inventory').fetch('worktrees').each do |record|
        next if record.fetch('removed')

        revalidate!(sidecar, journal:)
        next if record.fetch('removed')
        @runner.archive_cleanup_remove!(record.fetch('common_dir'), absolute(record.fetch('path')))
        prove_absent!(record)
        record['removed'] = true
        write(sidecar)
      end
      sidecar.fetch('inventory').fetch('containers').sort_by { |record| -record.fetch('path').count('/') - (record.fetch('path').empty? ? 0 : 1) }.each do |record|
        next if record.fetch('removed')

        revalidate!(sidecar, journal:)
        next if record.fetch('removed')
        Dir.rmdir(absolute(record.fetch('path')))
        fsync_directory(File.dirname(absolute(record.fetch('path'))))
        record['removed'] = true
        write(sidecar)
      end
      revalidate!(sidecar, journal:)
    rescue Errno::ENOTEMPTY, Errno::EEXIST => e
      raise Error, "archive cleanup container is no longer empty: #{e.message}"
    end

    def complete!(sidecar, journal)
      revalidate!(sidecar, journal:)
      unless sidecar.fetch('inventory').values_at('worktrees', 'containers').flatten.all? { |record| record.fetch('removed') }
        raise Error, "archive cleanup remains unfinished: #{@slug}"
      end
      sidecar['completed'] = true
      write(sidecar)
    end

    def discard_completed!(sidecar, recorded_journal: nil)
      raise Error, "archive cleanup is not completed: #{@slug}" unless sidecar.fetch('completed')

      journal = sidecar.slice(*BINDING_KEYS).merge(
        'schema' => 2, 'phase' => 'archived',
        'proven_heads' => sidecar.fetch('mode') == 'complete' ? sidecar.fetch('inventory').fetch('registered_heads') : {}
      )
      revalidate!(sidecar, journal:)
      if recorded_journal
        match_journal!(sidecar, recorded_journal)
        raise Error, 'archive retirement phases are unfinished' unless recorded_journal.fetch('phase') == 'archived'
      end
      @runner.archive_cleanup_check_completed!(@slug, journal, recorded_retirement: !recorded_journal.nil?)
      File.unlink(path)
      fsync_directory(File.dirname(path))
    end

    private

    def source_path
      File.join(@workspace, 'work', @slug)
    end

    def archive_path
      File.join(@workspace, 'archive', @slug)
    end

    def fsync_directory(value)
      File.open(value, File::RDONLY) { |directory| directory.fsync }
    end

    def reject_symlink_components!(value)
      current = Pathname.new('/')
      Pathname.new(value).each_filename do |component|
        current = current.join(component)
        break unless exists?(current.to_s)
        raise Error, "archive cleanup path has a symlink component: #{current}" if File.symlink?(current)
      end
    end

    def git(argv)
      stdout, = @runner.archive_cleanup_capture(argv)
      stdout
    end

    def resolve_commit(common, ref)
      head = git(['git', "--git-dir=#{common}", 'rev-parse', '--verify', "#{ref}^{commit}"]).strip
      raise Error, "archive ref is not a commit: #{ref}" unless head.match?(SHA)
      head
    end

    def fetch_refs!(common, refs)
      git(['git', "--git-dir=#{common}", 'fetch', '--no-tags', 'origin',
           *refs.uniq.map { |ref| "+refs/heads/#{ref}:refs/remotes/origin/#{ref}" }])
    end

    def valid_part?(value)
      value.is_a?(String) && value.match?(SAFE_PART)
    end

    def timestamp?(value)
      return false unless value.is_a?(String) && value.match?(/\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})\z/)
      Time.iso8601(value)
      true
    end

    def exists?(value)
      File.exist?(value) || File.symlink?(value)
    end

    def absolute(relative)
      relative.empty? ? @group : File.join(@group, relative)
    end

    def below_group?(value)
      value.start_with?(@group + '/')
    end

    def directory_identity(value)
      stat = File.lstat(value)
      raise Error, "archive cleanup directory is not an owned plain directory: #{value}" unless stat.directory? && stat.uid == Process.euid

      { 'dev' => stat.dev, 'ino' => stat.ino }
    rescue Errno::ENOENT, Errno::ENOTDIR => e
      raise Error, "archive cleanup directory is unavailable: #{e.message}"
    end

    def canonical_repositories
      sources = Dir[File.join(@workspace, 'repos', '*.git')].sort.map do |source|
        [File.basename(source, '.git'), @runner.archive_cleanup_repository(File.basename(source, '.git'))]
      end
      _stdout, _stderr, status = @runner.archive_cleanup_capture(
        ['git', '-C', @workspace, 'rev-parse', '--is-inside-work-tree'], allow_failure: true
      )
      if status.success?
        sources << ['workspace', @runner.archive_cleanup_repository('workspace')]
      end
      sources.uniq { |_project, common| common }
    end

    def registrations
      canonical_repositories.flat_map do |project, common|
        output = git(['git', "--git-dir=#{common}", 'worktree', 'list', '--porcelain', '-z'])
        output.split("\0\0").filter_map do |stanza|
          fields = stanza.split("\0")
          next if fields.empty?

          values = {}
          fields.each do |field|
            key, value = field.split(' ', 2)
            raise Error, "duplicate Git worktree field: #{key}" if values.key?(key)
            values[key] = value
          end
          raw_path = values.fetch('worktree') { raise Error, 'Git worktree registration has no path' }
          normalized = File.expand_path(raw_path)
          next unless below_group?(normalized)
          unless raw_path == normalized && !raw_path.match?(/[[:cntrl:]]/)
            raise Error, "invalid archive worktree registration path: #{raw_path.inspect}"
          end
          if values.key?('locked') || values.key?('prunable') || values.key?('bare')
            raise Error, "locked, prunable or missing archive worktree registration: #{raw_path}"
          end
          branch = values['branch']
          unless values['HEAD']&.match?(SHA) &&
                 ((branch&.start_with?('refs/heads/') && !values.key?('detached')) ||
                   (branch.nil? && values.key?('detached')))
            raise Error, "archive worktree has no shared branch or detached commit identity: #{raw_path}"
          end
          { 'path' => normalized, 'project' => project, 'common_dir' => common,
            'head' => values.fetch('HEAD'), 'branch' => branch&.delete_prefix('refs/heads/') }
        end
      end
    end

    def inspect_checkout(registration, clean:)
      checkout = registration.fetch('path')
      reject_symlink_components!(checkout)
      identity = directory_identity(checkout)
      top = git(['git', '-C', checkout, 'rev-parse', '--show-toplevel']).strip
      common = File.realpath(File.expand_path(git(['git', '-C', checkout, 'rev-parse', '--git-common-dir']).strip, checkout))
      admin = File.realpath(File.expand_path(git(['git', '-C', checkout, 'rev-parse', '--git-dir']).strip, checkout))
      git_file = File.join(checkout, '.git')
      unless top == checkout && common == registration.fetch('common_dir') &&
             File.dirname(admin) == File.join(common, 'worktrees') &&
             File.file?(git_file) && !File.symlink?(git_file) &&
             File.binread(git_file, 4097).strip == "gitdir: #{admin}" &&
             File.binread(File.join(admin, 'gitdir'), 4097).strip == git_file
        raise Error, "archive worktree Git administration does not match registration: #{checkout}"
      end
      head = git(['git', '-C', checkout, 'rev-parse', 'HEAD']).strip
      stdout, _stderr, status = @runner.archive_cleanup_capture(
        ['git', '-C', checkout, 'symbolic-ref', '--quiet', '--short', 'HEAD'], allow_failure: true
      )
      branch = status.success? ? stdout.strip : nil
      unless head == registration.fetch('head') && branch == registration.fetch('branch')
        raise Error, "archive worktree HEAD disagrees with registration: #{checkout}"
      end
      dirty = !git(['git', '-C', checkout, 'status', '--porcelain', '--untracked-files=all']).empty?
      raise Error, "worktree has uncommitted changes: #{checkout}" if clean && dirty

      registration.merge(
        'path' => checkout.delete_prefix(@group + '/'), 'identity' => identity,
        'common_identity' => directory_identity(common), 'admin_dir' => admin,
        'admin_identity' => directory_identity(admin), 'dirty' => dirty, 'removed' => false
      )
    end

    def walk_containers(directory, checkouts, containers)
      return if checkouts.include?(directory)

      reject_symlink_components!(directory)
      relative = directory == @group ? '' : directory.delete_prefix(@group + '/')
      unless relative.empty? || valid_relative?(relative)
        raise Error, "invalid archive container path: #{directory}"
      end
      containers << { 'path' => relative, 'identity' => directory_identity(directory), 'removed' => false }
      Dir.children(directory).sort.each do |name|
        child = File.join(directory, name)
        if File.symlink?(child) || !File.directory?(child)
          raise Error, "archive worktree group contains unmanaged entries: #{child}"
        end
        walk_containers(child, checkouts, containers)
      end
    end

    def current_manifest
      @runner.archive_cleanup_manifest(@slug)
    end

    def default_branch(common, repositories, required:)
      defaults = repositories.filter_map do |repository|
        repository.fetch('default_branch') if @runner.archive_cleanup_repository(repository.fetch('project')) == common
      end.uniq
      raise Error, "conflicting registered defaults for archive repository: #{common}" if defaults.length > 1

      output, _stderr, status = @runner.archive_cleanup_capture(
        ['git', "--git-dir=#{common}", 'symbolic-ref', '--quiet', 'refs/remotes/origin/HEAD'], allow_failure: true
      )
      value = output.strip.delete_prefix('refs/remotes/origin/')
      recorded_origin = status.success? && output.strip.start_with?('refs/remotes/origin/') && !value.empty?
      if defaults.length == 1
        if recorded_origin && value != defaults.first
          raise Error, "registered default conflicts with origin HEAD: #{common}"
        end
        return defaults.first
      end
      return value if recorded_origin
      raise Error, "archive repository has no recorded origin default; register it explicitly: #{common}" if required

      nil
    end

    def make_proof(kind, name, repository, common, mode, head: nil, allow_unpushed:)
      branch = repository['branch']
      default = repository['default_branch']
      origin = git(['git', "--git-dir=#{common}", 'config', '--get', 'remote.origin.url']).strip
      if repository['github'] && @runner.archive_cleanup_github_repository(origin) != repository.fetch('github')
        raise Error, "archive origin identity changed: #{name}"
      end
      head ||= resolve_commit(common, "refs/heads/#{branch}")
      if branch && resolve_commit(common, "refs/heads/#{branch}") != head
        raise Error, "archive checkout HEAD differs from retained branch: #{name}"
      end
      if mode == 'complete'
        unless default_branch(common, current_manifest&.fetch('repositories', []) || [], required: true) == default
          raise Error, "archive origin default does not match proof: #{name}"
        end
        if %w[registered additional].include?(kind)
          proof = @runner.archive_cleanup_branch_proof(repository, common, branch, default, allow_unpushed:)
          unless proof.fetch(:local_head) == head && (!proof.fetch(:feature_remote) || proof.fetch(:remote_head) == head)
            raise Error, "local and origin feature heads disagree: #{name}"
          end
          tip = proof.fetch(:default_head)
        else
          fetch_refs!(common, [default])
          tip = resolve_commit(common, "refs/remotes/origin/#{default}")
        end
        unless ancestor?(common, head, tip)
          raise Error, "feature head is not merged: #{name}: #{branch || head} -> origin/#{default}"
        end
        ref = "refs/remotes/origin/#{default}"
      elsif branch
        ref = "refs/heads/#{branch}"
        tip = head
      else
        ref, tip = retained_ref(common, head)
        raise Error, "orphan detached archive checkout #{name} at #{head}; retain it explicitly before archival" unless ref
      end
      {
        'kind' => kind, 'name' => name, 'project' => repository.fetch('project'),
        'common_dir' => common, 'common_identity' => directory_identity(common), 'origin' => origin,
        'branch' => branch, 'default_branch' => default, 'head' => head, 'ref' => ref, 'tip' => tip,
        'initial_base_sha' => repository['initial_base_sha'], 'allow_unpushed' => allow_unpushed
      }
    end

    def ancestor?(common, head, tip)
      _stdout, _stderr, status = @runner.archive_cleanup_capture(
        ['git', "--git-dir=#{common}", 'merge-base', '--is-ancestor', head, tip], allow_failure: true
      )
      return true if status.success?
      return false if status.exitstatus == 1

      raise Error, "archive retained ancestry is unavailable: #{head} -> #{tip}"
    end

    def retained_ref(common, head)
      refs = git(['git', "--git-dir=#{common}", 'for-each-ref', '--format=%(refname)',
                                 'refs/heads/', 'refs/tags/', 'refs/remotes/origin/']).lines.map(&:strip).sort
      refs.each do |ref|
        stdout, _stderr, status = @runner.archive_cleanup_capture(
          ['git', "--git-dir=#{common}", 'rev-parse', '--verify', "#{ref}^{commit}"], allow_failure: true
        )
        next unless status.success? && stdout.strip.match?(SHA)
        return [ref, stdout.strip] if ancestor?(common, head, stdout.strip)
      end
      nil
    end

    def reprove!(proof, mode)
      common = @runner.archive_cleanup_repository(proof.fetch('project'))
      unless common == proof.fetch('common_dir') && directory_identity(common) == proof.fetch('common_identity') &&
             git(['git', "--git-dir=#{common}", 'config', '--get', 'remote.origin.url']).strip == proof.fetch('origin')
        raise Error, "archive retained repository identity changed: #{proof.fetch('name')}"
      end
      shared_master = proof.fetch('kind') == 'registered' &&
                      @runner.archive_cleanup_shared_master_retained?(proof, common, proof.fetch('head'), mode:)
      if !shared_master && proof.fetch('branch') && resolve_commit(common, "refs/heads/#{proof.fetch('branch')}") != proof.fetch('head')
        detail = mode == 'complete' ? 'feature branch changed after merge proof' : 'feature branch changed during archival'
        raise Error, "#{detail}: #{proof.fetch('name')}"
      end
      if mode == 'complete'
        current_default = default_branch(common, current_manifest&.fetch('repositories', []) || [], required: true)
        unless current_default == proof.fetch('default_branch')
          raise Error, "archive origin default changed: #{proof.fetch('name')}"
        end
        if !shared_master && %w[registered additional].include?(proof.fetch('kind'))
          repository = proof.slice('branch', 'default_branch', 'initial_base_sha')
          actual = @runner.archive_cleanup_branch_proof(repository, common, proof.fetch('branch'), current_default,
                        allow_unpushed: proof.fetch('allow_unpushed'))
          unless actual.fetch(:local_head) == proof.fetch('head') &&
                 (!actual.fetch(:feature_remote) || actual.fetch(:remote_head) == proof.fetch('head'))
            raise Error, "feature head changed during archive cleanup: #{proof.fetch('name')}"
          end
        elsif !shared_master
          fetch_refs!(common, [current_default])
        end
      end
      tip = resolve_commit(common, proof.fetch('ref'))
      unless ancestor?(common, proof.fetch('head'), tip)
        raise Error, "archive retention ref no longer reaches sealed HEAD: #{proof.fetch('name')}"
      end
    end

    def prove_absent!(record)
      if exists?(absolute(record.fetch('path'))) || exists?(record.fetch('admin_dir')) ||
         registrations.any? { |registration| registration.fetch('path') == absolute(record.fetch('path')) }
        raise Error, "archive worktree path or registration remains after removal: #{absolute(record.fetch('path'))}"
      end
    end

    def inventory_digest(inventory)
      immutable = inventory.merge(
        'worktrees' => inventory.fetch('worktrees').map { |record| record.reject { |key, _value| key == 'removed' } },
        'containers' => inventory.fetch('containers').map { |record| record.reject { |key, _value| key == 'removed' } }
      )
      Digest::SHA256.hexdigest(JSON.generate(canonical(immutable)))
    end

    def canonical(value)
      case value
      when Hash then value.keys.sort.to_h { |key| [key, canonical(value.fetch(key))] }
      when Array then value.map { |item| canonical(item) }
      else value
      end
    end

    def keys?(value, keys)
      value.is_a?(Hash) && value.keys.sort == keys.sort
    end

    def valid_relative?(value, empty: false)
      value.is_a?(String) && (empty && value.empty? ||
        !value.empty? && !value.start_with?('/') && !value.match?(/[[:cntrl:]]/) &&
        value.split('/').none? { |part| ['', '.', '..'].include?(part) })
    end

    def identity?(value)
      keys?(value, %w[dev ino]) && value.values.all? { |part| part.is_a?(Integer) && part >= 0 }
    end

    def boolean?(value)
      value == true || value == false
    end

    def validate!(sidecar)
      inventory = sidecar['inventory'] if sidecar.is_a?(Hash)
      valid = keys?(sidecar, KEYS) && sidecar['schema'] == 1 && sidecar['sealed'] == true &&
        sidecar['workspace'] == @workspace && sidecar['slug'] == @slug &&
        sidecar['operation_id'].is_a?(String) && sidecar['operation_id'].match?(DIGEST) &&
        %w[complete abandoned].include?(sidecar['mode']) &&
        timestamp?(sidecar['finalized_at']) &&
        sidecar['target_tracking_sha256'].is_a?(String) && sidecar['target_tracking_sha256'].match?(DIGEST) &&
        (sidecar['retained_thread_id'].nil? || sidecar['retained_thread_id'].is_a?(String) && sidecar['retained_thread_id'].match?(/\A[0-9A-Za-z-]+\z/)) &&
        boolean?(sidecar['completed']) && keys?(inventory, INVENTORY_KEYS) &&
        identity?(inventory['tracking_identity']) && inventory['source_sha256'].is_a?(String) && inventory['source_sha256'].match?(DIGEST) &&
        inventory['registered_heads'].is_a?(Hash) && inventory['registered_heads'].all? { |name, head| valid_part?(name) && head.is_a?(String) && head.match?(SHA) } &&
        inventory['worktrees'].is_a?(Array) && inventory['containers'].is_a?(Array) && inventory['proofs'].is_a?(Array)
      if valid
        valid &&= inventory['worktrees'].all? do |record|
          keys?(record, WORKTREE_KEYS) && valid_relative?(record['path']) && valid_part?(record['project']) &&
            record['common_dir'] == @runner.archive_cleanup_repository(record['project']) &&
            record['admin_dir'].is_a?(String) && File.dirname(record['admin_dir']) == File.join(record['common_dir'], 'worktrees') &&
            %w[identity admin_identity common_identity].all? { |key| identity?(record[key]) } &&
            record['head'].is_a?(String) && record['head'].match?(SHA) &&
            (record['branch'].nil? || valid_branch?(record['branch'])) && record['dirty'] == false && boolean?(record['removed'])
        end
        valid &&= inventory['containers'].all? { |record| keys?(record, %w[path identity removed]) && valid_relative?(record['path'], empty: true) && identity?(record['identity']) && boolean?(record['removed']) }
        valid &&= inventory['proofs'].all? do |proof|
          keys?(proof, PROOF_KEYS) && %w[registered additional default detached].include?(proof['kind']) &&
            valid_relative?(proof['name']) && valid_part?(proof['project']) &&
            proof['common_dir'] == @runner.archive_cleanup_repository(proof['project']) && identity?(proof['common_identity']) &&
            proof['origin'].is_a?(String) && !proof['origin'].empty? && !proof['origin'].include?("\0") &&
            (proof['branch'].nil? || valid_branch?(proof['branch'])) &&
            (proof['default_branch'].nil? || valid_branch?(proof['default_branch'])) &&
            %w[head tip].all? { |key| proof[key].is_a?(String) && proof[key].match?(SHA) } &&
            valid_retained_ref?(proof['ref']) &&
            (proof['initial_base_sha'].nil? || proof['initial_base_sha'].is_a?(String) && proof['initial_base_sha'].match?(SHA)) &&
            boolean?(proof['allow_unpushed'])
        end
        # Cross-record checks can index rows only after every row's shape has
        # been proved. Malformed persisted input must remain a normal refusal.
        raise Error, "invalid archive cleanup sidecar: #{path}" unless valid

        valid &&= %w[worktrees containers].all? { |kind| inventory[kind].map { |record| record['path'] }.uniq.length == inventory[kind].length }
        proof_names = inventory['proofs'].map { |proof| proof['name'] }
        registered = inventory['proofs'].select { |proof| proof['kind'] == 'registered' }
        valid &&= proof_names.uniq.length == proof_names.length &&
          registered.to_h { |proof| [proof['name'], proof['head']] } == inventory['registered_heads']
        valid &&= inventory['worktrees'].all? do |record|
          proof = inventory['proofs'].find { |item| item['name'] == record['path'] }
          proof && %w[common_dir head branch].all? { |key| proof[key] == record[key] }
        end
        valid &&= inventory['proofs'].all? do |proof|
          if proof['kind'] == 'detached'
            proof['branch'].nil? && !proof['allow_unpushed']
          else
            !proof['branch'].nil? && (proof['kind'] == 'registered' || !proof['allow_unpushed'])
          end && (sidecar['mode'] != 'complete' || !proof['default_branch'].nil?)
        end
        valid &&= sidecar['inventory_sha256'] == inventory_digest(inventory)
        valid &&= !sidecar['completed'] || inventory.values_at('worktrees', 'containers').flatten.all? { |record| record['removed'] }
      end
      raise Error, "invalid archive cleanup sidecar: #{path}" unless valid
    rescue KeyError, TypeError, ArgumentError => e
      raise Error, "invalid archive cleanup sidecar: #{e.message}"
    end

    def valid_branch?(value)
      return false unless value.is_a?(String) && !value.empty?

      _stdout, _stderr, status = @runner.archive_cleanup_capture(
        ['git', 'check-ref-format', "refs/heads/#{value}"], allow_failure: true
      )
      status.success? && value != 'HEAD' && !value.start_with?('-')
    end

    def valid_retained_ref?(value)
      return false unless value.is_a?(String) && value.match?(%r{\Arefs/(?:heads/|tags/|remotes/origin/)})

      _stdout, _stderr, status = @runner.archive_cleanup_capture(
        ['git', 'check-ref-format', value], allow_failure: true
      )
      status.success?
    end

    def write(sidecar, create: false)
      validate!(sidecar)
      encoded = JSON.generate(sidecar) + "\n"
      raise Error, "archive cleanup inventory exceeds 1 MiB: #{@slug}" if encoded.bytesize > MAX_BYTES
      raise Error, "archive cleanup sidecar already exists: #{@slug}" if create && exists?(path)

      temporary = "#{path}.#{$$}.tmp"
      File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
        file.write(encoded)
        file.flush
        file.fsync
      end
      File.rename(temporary, path)
      fsync_directory(File.dirname(path))
    ensure
      File.unlink(temporary) if temporary && exists?(temporary)
    end
  end
end
