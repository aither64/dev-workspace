# frozen_string_literal: true

module DevSession
  # File and index recovery for explicit archival. This storage is private and
  # local; Git branches and object refs are retained without publication checks.
  class ArchiveRecovery
    ADMIN_EXCLUDES = %w[gitdir commondir HEAD logs].freeze

    def initialize(root, group, command_runner, progress: nil)
      @root = root
      @group = group
      @commands = command_runner
      @progress = progress
    end

    def capture!(records)
      FileUtils.mkdir_p(@root, mode: 0o700)
      File.chmod(0o700, @root)
      records.each do |record|
        @progress&.call('capturing checkout', record.fetch('path'))
        key = Digest::SHA256.hexdigest(record.fetch('path'))
        destination = File.join(@root, key)
        if File.file?(File.join(destination, 'record.json'))
          verify_checkout!(record)
          next
        end
        FileUtils.rm_rf(destination) if File.directory?(destination)
        FileUtils.mkdir_p(File.join(destination, 'files'), mode: 0o700)
        FileUtils.mkdir_p(File.join(destination, 'admin'), mode: 0o700)
        source = File.join(@group, record.fetch('path'))
        git_capture = git(record, 'status', '--porcelain', '--untracked-files=all',
                          '--ignored=matching', checkout: source).empty?
        # Git status compares filtered content. Only raw blob equality makes
        # reconstruction independent of filters, LFS and line-ending settings.
        git_capture &&= raw_git_contents?(record, source)
        copy_contents(source, File.join(destination, 'files'), excludes: ['.git']) unless git_capture
        copy_contents(record.fetch('admin_dir'), File.join(destination, 'admin'), excludes: ADMIN_EXCLUDES)
        ref_prefix = "refs/dev-session-archives/#{File.basename(@root)}/#{key}"
        retained = git(record, '--no-replace-objects', 'ls-tree', '-r', '--format=%(objectname)', record.fetch('head')).lines.to_h do |line|
          [line.strip, true]
        end
        index_objects = git(record, 'ls-files', '--stage', '-z', checkout: source).split("\0").filter_map do |entry|
          next if entry.start_with?('160000 ')
          match = entry.match(/\A[0-9]+ ([0-9a-f]{40}|[0-9a-f]{64}) [0-3]\t/)
          match && match[1] unless match && match[1].match?(/\A0+\z/)
        end.uniq
        updates = { "#{ref_prefix}/head" => record.fetch('head') }
        index_objects.reject { |object| retained[object] }.each do |object|
          updates["#{ref_prefix}/index/#{object}"] = object
        end
        operation_objects = []
        Dir.glob(File.join(destination, 'admin', '**', '*')).each do |path|
          next unless File.file?(path) && !File.symlink?(path) && File.size(path) <= 1024 * 1024
          next if File.basename(path).start_with?('index', 'sharedindex')

          operation_objects.concat(File.binread(path).scan(/\b(?:[0-9a-f]{64}|[0-9a-f]{40})\b/))
        end
        unless operation_objects.empty?
          output, = @commands.capture(
            ['git', "--git-dir=#{record.fetch('common_dir')}", 'cat-file', '--batch-check'],
            input: operation_objects.uniq.join("\n") + "\n"
          )
          output.each_line do |line|
            object, kind, = line.split
            updates["#{ref_prefix}/operation/#{object}"] = object unless kind == 'missing'
          end
        end
        @commands.capture(
          ['git', "--git-dir=#{record.fetch('common_dir')}", 'update-ref', '--stdin'],
          input: updates.map { |ref, object| "update #{ref} #{object}\n" }.join
        )
        saved = record.reject { |name, _| name == 'removed' }.merge(
          'format' => 2,
          'kind' => git_capture ? 'git' : 'files',
          'files_sha256' => tree_digest(git_capture ? source : File.join(destination, 'files'), excludes: git_capture ? ['.git'] : []),
          'admin_sha256' => tree_digest(File.join(destination, 'admin')),
          'head_ref' => "#{ref_prefix}/head"
        )
        saved['permissions'] = permissions(source) if git_capture
        sync_tree(destination)
        write_json(File.join(destination, 'record.json'), saved)
        verify_checkout!(record)
      end
      write_json(File.join(@root, 'inventory.json'), { 'schema' => 2, 'worktrees' => records })
    end

    def records
      inventory = JSON.parse(File.binread(File.join(@root, 'inventory.json')))
      unless [1, 2].include?(inventory['schema']) && inventory['worktrees'].is_a?(Array)
        raise Error, 'invalid archive recovery inventory'
      end
      inventory.fetch('worktrees').each do |record|
        path = record.fetch('path')
        unless path.is_a?(String) && !path.empty? && !Pathname.new(path).absolute? &&
               path.split('/').none? { |part| part.empty? || %w[. .. .git].include?(part) }
          raise Error, 'invalid archive recovery checkout path'
        end
        saved, destination = read_record(record)
        verify_snapshot!(saved, destination)
      end
      inventory.fetch('worktrees')
    end

    def verify_checkout!(record)
      @progress&.call('verifying checkout', record.fetch('path'))
      saved, destination = read_record(record)
      source = File.join(@group, record.fetch('path'))
      unless tree_digest(source, excludes: ['.git']) == saved.fetch('files_sha256') &&
             tree_digest(record.fetch('admin_dir'), excludes: ADMIN_EXCLUDES) == saved.fetch('admin_sha256')
        raise Error, "worktree changed after archive recovery capture: #{source}"
      end
      verify_snapshot!(saved, destination)
    end

    def verify_saved!(record)
      saved, destination = read_record(record)
      verify_snapshot!(saved, destination)
    end

    def verify_snapshot!(saved, destination)
      files_match = if saved['kind'] == 'git'
                      git(saved, '--no-replace-objects', 'rev-parse', '--verify', saved.fetch('head_ref')).strip == saved.fetch('head') &&
                        verify_git_objects!(saved)
                    else
                      tree_digest(File.join(destination, 'files')) == saved.fetch('files_sha256')
                    end
      unless files_match &&
             tree_digest(File.join(destination, 'admin')) == saved.fetch('admin_sha256')
        raise Error, "archive recovery contents changed: #{destination}"
      end
    end

    def restore!(record)
      saved, destination = read_record(record)
      verify_snapshot!(saved, destination)
      target = File.join(@group, record.fetch('path'))
      receipt = File.join(destination, 'restored.json')
      restoring = File.join(destination, 'restoring.json')
      if File.file?(receipt)
        # Completion is irreversible even if interruption left the intent.
        # Later edits belong to the operator and must never be replaced.
        unless restored?(record, saved)
          raise Error, "completed archive recovery checkout changed: #{target}"
        end
        if File.file?(restoring)
          File.unlink(restoring)
          File.open(destination, File::RDONLY) { |directory| directory.fsync }
        end
        return
      end
      if File.exist?(target) && !File.file?(restoring)
        raise Error, "archive recovery checkout already exists: #{target}"
      end
      unless File.file?(restoring)
        write_json(restoring, record.slice('path', 'common_dir', 'head', 'branch'))
      end
      unless File.directory?(target)
        FileUtils.mkdir_p(File.dirname(target), mode: 0o700)
        branch = record['branch']
        branch_head, _err, status = @commands.capture(
          ['git', "--git-dir=#{record.fetch('common_dir')}", 'rev-parse', '--verify', "refs/heads/#{branch}"], allow_failure: true
        ) if branch
        arguments = if branch && status.success? && branch_head.strip == record.fetch('head')
                      ['worktree', 'add', '--no-checkout', target, branch]
                    else
                      ['worktree', 'add', '--no-checkout', '--detach', target, saved.fetch('head_ref')]
                    end
        git(record, *arguments)
      end
      common = File.realpath(File.expand_path(capture(['git', '-C', target, 'rev-parse', '--git-common-dir']).strip, target))
      raise Error, "archive recovery repository changed: #{target}" unless common == record.fetch('common_dir')
      unless capture(['git', '-C', target, 'rev-parse', 'HEAD']).strip == record.fetch('head')
        raise Error, "archive recovery checkout head changed: #{target}"
      end

      admin = File.realpath(File.expand_path(capture(['git', '-C', target, 'rev-parse', '--git-dir']).strip, target))
      # Restoration owns this newly created checkout. Replacing its files never
      # resets another user's checkout or a shared repository index.
      if saved['kind'] == 'git'
        # The private intent owns this incomplete checkout until restored.json.
        # Rebuild raw bytes on every retry, independent of checkout filters.
        Dir.children(target).each { |name| FileUtils.rm_rf(File.join(target, name)) unless name == '.git' }
        restore_git_contents!(saved, target)
        restore_permissions(target, saved.fetch('permissions'))
      else
        Dir.children(target).each { |name| FileUtils.rm_rf(File.join(target, name)) unless name == '.git' }
        copy_contents(File.join(destination, 'files'), target)
      end
      copy_contents(File.join(destination, 'admin'), admin)
      unless restored?(record, saved)
        raise Error, "archive recovery restore verification failed: #{target}"
      end
      sync_tree(target, excludes: ['.git'])
      sync_tree(admin, excludes: ADMIN_EXCLUDES)
      write_json(receipt, { 'path' => record.fetch('path'), 'files_sha256' => saved.fetch('files_sha256') })
      File.unlink(restoring)
      File.open(destination, File::RDONLY) { |directory| directory.fsync }
    end

    private

    def verify_git_objects!(saved)
      # Restrict traversal to the saved tree, not the repository's full history.
      objects = git(saved, '--no-replace-objects', 'rev-list', '--objects', '--no-object-names', "#{saved.fetch('head')}^{tree}").lines.map(&:strip)
      objects.unshift(saved.fetch('head'))
      stream_git_objects(saved, objects) { |_object, _kind, _size, _stream| }
      true
    end

    def restore_git_contents!(saved, target)
      entries = git(saved, '--no-replace-objects', 'ls-tree', '-r', '-z', saved.fetch('head')).split("\0").map do |entry|
        metadata, relative = entry.split("\t", 2)
        mode, kind, object = metadata.split
        unless kind == 'blob' && relative && !Pathname.new(relative).absolute? &&
               relative.split('/').none? { |part| part.empty? || %w[. .. .git].include?(part) }
          raise Error, 'invalid Git recovery tree entry'
        end
        [mode, relative, object]
      end
      index = 0
      stream_git_objects(saved, entries.map(&:last)) do |_object, kind, size, read|
        mode, relative, = entries.fetch(index)
        index += 1
        raise Error, 'invalid Git recovery blob' unless kind == 'blob'

        path = File.join(target, relative)
        FileUtils.mkdir_p(File.dirname(path))
        if mode == '120000'
          raise Error, 'oversized Git recovery symlink' if size > 1024 * 1024

          File.symlink(read.call(size), path)
        else
          File.open(path, 'wb', 0o600) do |file|
            while (chunk = read.call(1024 * 1024))
              file.write(chunk)
            end
          end
        end
      end
    end

    # Git owns object decoding. Check the raw object identity while consuming
    # one bounded stream; a missing or corrupt blob must never permit removal.
    def stream_git_objects(saved, objects)
      return if objects.empty?

      @commands.stream(['git', '--no-replace-objects', "--git-dir=#{saved.fetch('common_dir')}", 'cat-file', '--batch'], input: objects.join("\n") + "\n") do |stream|
        objects.each do |object|
          header = stream.gets&.split
          unless header && header.length == 3 && header[0] == object && %w[blob tree commit].include?(header[1]) && header[2].match?(/\A[0-9]+\z/)
            raise Error, "archive recovery Git object is missing or unreadable: #{object}"
          end
          size = Integer(header[2])
          digest = object.length == 40 ? Digest::SHA1.new : Digest::SHA256.new
          digest << "#{header[1]} #{size}\0".b
          remaining = size
          read = lambda do |length|
            next nil if remaining.zero?

            chunk = stream.read([length, remaining].min)
            raise Error, 'truncated archive recovery Git object' if chunk.nil? || chunk.empty?

            digest << chunk
            remaining -= chunk.bytesize
            chunk
          end
          yield object, header[1], size, read
          nil while read.call(1024 * 1024)
          unless stream.read(1) == "\n" && digest.hexdigest == object
            raise Error, "archive recovery Git object is corrupt: #{object}"
          end
        end
      end
    end

    def raw_git_contents?(record, source)
      digest_class = record.fetch('head').size == 40 ? Digest::SHA1 : Digest::SHA256
      git(record, '--no-replace-objects', 'ls-tree', '-r', '-z', record.fetch('head')).split("\0").all? do |entry|
        metadata, relative = entry.split("\t", 2)
        mode, kind, object = metadata.split
        next false unless kind == 'blob'

        path = File.join(source, relative)
        if File.symlink?(path)
          next false unless mode == '120000'

          value = File.readlink(path).b
          digest_class.hexdigest("blob #{value.bytesize}\0".b + value) == object
        elsif File.file?(path)
          next false unless %w[100644 100755].include?(mode)

          digest = digest_class.new
          digest << "blob #{File.size(path)}\0"
          File.open(path, 'rb') do |file|
            while (chunk = file.read(1024 * 1024))
              digest << chunk
            end
          end
          digest.hexdigest == object
        else
          false
        end
      end
    end

    def permissions(root)
      result = {}
      walk = lambda do |directory, prefix|
        Dir.children(directory).sort.each do |name|
          next if prefix.empty? && name == '.git'

          relative = prefix.empty? ? name : File.join(prefix, name)
          path = File.join(root, relative)
          stat = File.lstat(path)
          result[relative] = [stat.ftype, stat.mode & 0o777]
          walk.call(path, relative) if stat.directory? && !stat.symlink?
        end
      end
      walk.call(root, '')
      result
    end

    def restore_permissions(root, entries)
      entries.each do |relative, (kind, mode)|
        unless relative.is_a?(String) && !Pathname.new(relative).absolute? &&
               relative.split('/').none? { |part| part.empty? || %w[. .. .git].include?(part) } &&
               %w[file directory link].include?(kind) && mode.is_a?(Integer) && mode.between?(0, 0o777)
          raise Error, 'invalid Git recovery permission entry'
        end
        path = File.join(root, relative)
        FileUtils.mkdir_p(path) if kind == 'directory'
        next if kind == 'link'

        File.chmod(mode, path)
      end
    end

    def restored?(record, saved)
      target = File.join(@group, record.fetch('path'))
      return false unless File.directory?(target) && !File.symlink?(target)

      admin = File.realpath(File.expand_path(capture(['git', '-C', target, 'rev-parse', '--git-dir']).strip, target))
      capture(['git', '-C', target, 'rev-parse', 'HEAD']).strip == record.fetch('head') &&
        tree_digest(target, excludes: ['.git']) == saved.fetch('files_sha256') &&
        tree_digest(admin, excludes: ADMIN_EXCLUDES) == saved.fetch('admin_sha256')
    end

    def read_record(record)
      destination = File.join(@root, Digest::SHA256.hexdigest(record.fetch('path')))
      saved = JSON.parse(File.binread(File.join(destination, 'record.json')))
      if saved.key?('format') && (saved['format'] != 2 || !%w[git files].include?(saved['kind']))
        raise Error, 'unsupported archive recovery record format'
      end
      if saved['kind'] == 'git' && !saved['permissions'].is_a?(Hash)
        raise Error, 'Git recovery permissions are missing'
      end
      %w[path common_dir head branch].each do |key|
        raise Error, "archive recovery identity changed: #{record.fetch('path')}" unless saved[key] == record[key]
      end
      [saved, destination]
    end

    def copy_contents(source, destination, excludes: [])
      Dir.children(source).sort.each do |name|
        next if excludes.include?(name)

        FileUtils.copy_entry(File.join(source, name), File.join(destination, name), true, false, true)
      end
    end

    def tree_digest(root, excludes: [])
      digest = Digest::SHA256.new
      walk = lambda do |directory, relative|
        Dir.children(directory).sort.each do |name|
          next if relative.empty? && excludes.include?(name)

          path = File.join(directory, name)
          part = relative.empty? ? name : File.join(relative, name)
          stat = File.lstat(path)
          digest << JSON.generate([part, stat.ftype, stat.mode & 0o777]) << "\n"
          if stat.symlink?
            digest << File.readlink(path) << "\0"
          elsif stat.file?
            digest << Digest::SHA256.file(path).hexdigest << "\n"
          elsif stat.directory?
            walk.call(path, part)
          else
            raise Error, "unsupported file in archive recovery: #{path}"
          end
        end
      end
      walk.call(root, '')
      digest.hexdigest
    end

    def sync_tree(root, excludes: [])
      Dir.children(root).each do |name|
        next if excludes.include?(name)

        path = File.join(root, name)
        next if File.symlink?(path)

        if File.directory?(path)
          sync_tree(path)
        elsif File.file?(path)
          File.open(path, File::RDONLY) { |file| file.fsync }
        end
      end
      File.open(root, File::RDONLY) { |directory| directory.fsync }
    end

    def write_json(path, record)
      temporary = "#{path}.#{$$}.tmp"
      File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
        file.write(JSON.generate(record) + "\n")
        file.flush
        file.fsync
      end
      File.rename(temporary, path)
      File.open(File.dirname(path), File::RDONLY) { |directory| directory.fsync }
    ensure
      File.unlink(temporary) if temporary && File.exist?(temporary)
    end

    def capture(arguments)
      stdout, = @commands.capture(arguments)
      stdout
    end

    def git(record, *arguments, checkout: nil)
      prefix = checkout ? ['git', '-C', checkout] : ['git', "--git-dir=#{record.fetch('common_dir')}"]
      capture(prefix + arguments)
    end
  end
end
