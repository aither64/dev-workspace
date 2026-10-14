# frozen_string_literal: true

# Session records stay in the usual workspace layout. The private record is
# durable across runtime retirement and a tombstone after local archival.
module DevSession
  module SensitiveSessions
    private

    def write_sensitive_file!(path, content)
      temporary = Tempfile.new(['.sensitive-', '.tmp'], File.dirname(path))
      begin
        temporary.chmod(0o600)
        temporary.write(content)
        temporary.flush
        temporary.fsync
        File.rename(temporary.path, path)
        fsync_directory(File.dirname(path))
      ensure
        temporary.close!
      end
    end

    def sensitivity_file(slug)
      id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      File.join(@workspace_state_root, 'sensitive-sessions', id, "#{slug}.json")
    end

    def sensitive_slugs
      Dir[File.join(File.dirname(sensitivity_file('placeholder')), '*.json')].map do |path|
        slug = File.basename(path, '.json')
        raise Error, 'invalid private sensitive session slug' unless valid_part?(slug)
        sensitivity_record(slug)
        slug
      end
    end

    def sensitivity_record(slug)
      path = sensitivity_file(slug)
      return nil unless path_exists?(path)

      record = read_private_json_object(path, 'session sensitivity', max_size: 4096, mode: 0o600)
      unless record.keys.sort == %w[archived schema slug workspace] && record['schema'] == 1 &&
             record['slug'] == slug && record['workspace'] == workspace &&
             [true, false].include?(record['archived'])
        raise Error, "invalid session sensitivity: #{slug}"
      end
      record
    end

    def sensitive_session?(slug)
      !sensitivity_record(slug).nil?
    end

    def write_sensitivity_record!(slug, record)
      path = sensitivity_file(slug)
      FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
      write_sensitive_file!(path, JSON.generate(record) + "\n")
    end

    def select_sensitivity!(slug, requested: nil)
      existing = sensitivity_record(slug)
      if requested && !existing
        if path_exists?(work_dir(slug)) || path_exists?(archive_dir(slug)) ||
           path_exists?(creation_journal_file(slug)) || path_exists?(fork_journal_file(slug))
          raise Error, 'sensitivity can only be selected for a new session'
        end
        # Never hide content which is already present in any retained Git ref.
        if workspace_git_repository? && !safe_workspace_git_capture('log', '--all', '--max-count=1', '--format=%H', '--', "work/#{slug}", "archive/#{slug}").empty?
          raise Error, 'sensitive session paths have existing Git history'
        end
        existing = { 'schema' => 1, 'workspace' => workspace, 'slug' => slug, 'archived' => false }
        # Publish the immutable choice before any tracking or conversation writes.
        write_sensitivity_record!(slug, existing)
      end
      if existing
        ensure_sensitive_exclusions!(slug)
        [work_dir(slug), archive_dir(slug)].each do |directory|
          path = File.join(directory, PORTAL_MANIFEST)
          next unless path_exists?(path)
          manifest = load_portal_manifest(path, required: true, expected_slug: slug)
          raise Error, 'session sensitivity marker changed' unless manifest['sensitive'] == true
        end
      end
    end

    def ensure_sensitive_exclusions!(slug)
      raise Error, 'sensitive sessions require a Git workspace for local exclusions' unless workspace_git_repository?

      with_tracking_commit_lock do
        roots = ["work/#{slug}", "archive/#{slug}", "worktrees/#{slug}", "worktrees/.locks/#{slug}.*"]
        patterns = roots.first(3).map { |root| "/#{root}/" } + ["/#{roots.last}"]
        unless safe_workspace_git_capture('ls-files', '-z', '--', *roots).empty? &&
               roots.none? { |root| committed_path?('HEAD', root) }
          raise Error, "sensitive session records are tracked or staged: #{slug}"
        end
        path = git_capture(['git', '-C', workspace, 'rev-parse', '--path-format=absolute', '--git-path', 'info/exclude']).strip
        FileUtils.mkdir_p(File.dirname(path))
        content = File.exist?(path) ? File.binread(path) : ''.b
        lines = content.lines.map(&:chomp)
        additions = patterns.reject { |line| lines.include?(line) }
        unless additions.empty?
          content += "\n" unless content.empty? || content.end_with?("\n")
          write_sensitive_file!(path, content + additions.join("\n") + "\n")
        end
        journals = [creation_journal_file(slug), fork_journal_file(slug), start_journal_file(slug),
                    *LIFECYCLE_JOURNAL_NAMES.keys.map { |command| lifecycle_journal_file(slug, command) },
                    ArchiveCleanup.new(self, slug).path]
        # Git rules can override one file without overriding its siblings. Probe
        # each owning writer's real destination and its atomic-write temporary.
        probes = roots.first(3).map { |root| "#{root}/" }
        probes.concat(journals.flat_map { |journal| [journal, "#{journal}.#{$$}.tmp"] })
        probes.concat(Dir[File.join(workspace, 'worktrees', '.locks', "#{slug}.*")])
        probes.each do |root|
          _out, _err, status = @command_runner.capture(['git', '-C', workspace, 'check-ignore', '--quiet', '--', root], allow_failure: true)
          raise Error, "Git exclusion is overridden for sensitive records: #{root}" unless status.success?
        end
      end
    end

    def validate_sensitive_transition!(slug, direction:)
      ensure_sensitive_exclusions!(slug)
      source, destination = direction == 'archive' ? [work_dir(slug), archive_dir(slug)] : [archive_dir(slug), work_dir(slug)]
      unless !path_exists?(source) && File.directory?(destination)
        raise Error, "local sensitive tracking transition is incomplete: #{slug}"
      end
      manifest = load_portal_manifest(File.join(destination, PORTAL_MANIFEST), required: true, expected_slug: slug)
      raise Error, 'sensitive tracking lost its sensitivity marker' unless manifest['sensitive'] == true
      record = sensitivity_record(slug)
      archived = direction == 'archive'
      write_sensitivity_record!(slug, record.merge('archived' => archived)) if record['archived'] != archived
    end
  end
end
