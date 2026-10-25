# frozen_string_literal: true

module DevSession
  # Readiness is advisory. The ordinary executor repeats its checks while it
  # owns lifecycle locks; this reader never pins refs or publishes an intent.
  module ArchiveCheck
    def archive_check(input, as_is:)
      slug = lookup_slug(input, as_is:)
      blockers = []
      count = 0
      passed = 0
      total = 0
      stage = 0
      advancing = true
      inventory = nil
      add = lambda do |code, message, path = nil|
        count += 1
        blockers << { 'code' => code, 'message' => message.to_s.byteslice(0, 2048).scrub,
                      'path' => path } if blockers.length < 128
      end
      check = lambda do |code, &block|
        total += 1
        before = count
        begin
          block.call
        rescue Error, WorkspaceCoordination::Error, SystemCallError => e
          add.call(code, e.message)
        end
        if count == before
          passed += 1
          stage += 1 if advancing
        else
          advancing = false
        end
      end
      check.call('session') do
        select_tmux_for_slug!(slug, read_only: true)
        reject_conflicting_lifecycle_journal!(slug)
        source = validate_work_directory!(slug)
        raise Error, 'archive destination already exists' if path_exists?(archive_dir(slug))
        raise Error, 'session tracking is not writable' unless File.writable?(source) && File.writable?(File.dirname(source))
        require_ordinary_manifest_ready!(slug, load_portal_manifest(portal_file(slug), required: true))
        validate_tracking_files!(slug)
      end
      check.call('worktrees') do
        inventory = ArchiveCleanup.new(self, slug).discover(clean: false, on_unmanaged: lambda do |path|
          add.call('unmanaged_entry', "Move this entry inside its owning checkout or outside the worktree group: #{path}", path)
        end)
      end
      check.call('conversation') do
        selected = cleanup_session(slug)
        if selected && @authority_dir
          raise Error, "trusted session authority is missing for #{slug}" unless @selected_authority
          validate_authority_session!(slug, selected, require_creation_identity: true)
        end
        manifest = load_portal_manifest(portal_file(slug), required: true)
        if manifest.dig('codex', 'thread_id')
          ensure_archive_conversations_ready!(slug, archive_mode: 'archived')
          raise Error, 'native identity checker is unavailable' unless @portal_command && !@portal_command.empty?
          team(slug, action: 'require-archive-identities', lifecycle: true)
        else
          require_threadless_archive_tracking!(slug, manifest, explicit: true)
          require_threadless_archive_runtime!(slug)
        end
      end
      check.call('filesystem') do
        source = validate_work_directory!(slug)
        destination = File.directory?(File.join(workspace, 'archive')) ? File.join(workspace, 'archive') : workspace
        raise Error, 'work and archive directories must be on the same filesystem' unless File.stat(source).dev == File.stat(destination).dev
        raise Error, 'archive parent is not writable' unless File.writable?(destination)
        validate_development_cluster_helpers!
        validate_archive_move_command!
        if inventory
          group = File.join(workspace, 'worktrees', slug)
          containers = inventory.fetch('containers').map { |record| record.fetch('path').empty? ? group : File.join(group, record.fetch('path')) }
          checkouts = inventory.fetch('worktrees').map { |record| File.join(group, record.fetch('path')) }
          administrations = inventory.fetch('worktrees').map { |record| record.fetch('admin_dir') }
          (containers + checkouts + administrations).map { |path| File.dirname(path) }.uniq.each do |parent|
            next if File.writable?(parent) && File.executable?(parent)

            add.call('removal_parent_permission', "Archive needs write and search access to this removal parent: #{parent}", parent)
          end
        end
        inventory&.fetch('worktrees')&.each do |record|
          directories = [File.join(workspace, 'worktrees', slug, record.fetch('path'))]
          until directories.empty?
            directory = directories.pop
            unless File.writable?(directory) && File.executable?(directory)
              add.call('removal_permission', "Make this session-owned directory writable and searchable before archival: #{directory}", directory)
              next
            end
            Dir.each_child(directory) do |name|
              child = File.join(directory, name)
              stat = File.lstat(child)
              directories << child if stat.directory? && !stat.symlink?
            rescue Errno::ENOENT
              next
            end
          end
        end
      end
      check.call('tracking') do
        # The sensitive-session mutation helper installs exclusions. Observation
        # must not call it. Final archival retains its normal sensitive checks.
        unless sensitive_session?(slug)
          validate_tracking_committed!(slug) unless defer_tracking?
          validate_tracking_transition_commit!(slug, refresh: false, local_only: true)
        end
      end
      check.call('coordination') do
        coordination_store.require_lifecycle_idle!(workspace, slug)
        coordination_store.require_lifecycle_capacity!(workspace, slug, 'archive')
      end
      { 'schema' => 1, 'workspace' => workspace, 'slug' => slug, 'ready' => count.zero?,
        'passedChecks' => passed, 'totalChecks' => total, 'readyStage' => stage, 'blockerCount' => count, 'blockers' => blockers }
    end

    def require_archive_check!(slug)
      result = archive_check(slug, as_is: true)
      return if result.fetch('ready')

      raise Error, "archive readiness checks failed:\n- #{result.fetch('blockers').map { |item| item.fetch('message') }.join("\n- ")}"
    end
  end
end
