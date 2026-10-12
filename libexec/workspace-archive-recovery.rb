# frozen_string_literal: true

module DevSession
  # File and index recovery for explicit archival. This storage is private and
  # local; Git branches and object refs are retained without publication checks.
  class ArchiveRecovery
    ADMIN_EXCLUDES = %w[gitdir commondir HEAD logs].freeze

    def initialize(root, group, command_runner)
      @root = root
      @group = group
      @commands = command_runner
    end

    def capture!(records)
      FileUtils.mkdir_p(@root, mode: 0o700)
      File.chmod(0o700, @root)
      records.each do |record|
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
        copy_contents(source, File.join(destination, 'files'), excludes: ['.git'])
        copy_contents(record.fetch('admin_dir'), File.join(destination, 'admin'), excludes: ADMIN_EXCLUDES)
        ref_prefix = "refs/dev-session-archives/#{File.basename(@root)}/#{key}"
        git(record, 'update-ref', "#{ref_prefix}/head", record.fetch('head'))
        index_objects = git(record, 'ls-files', '--stage', '-z', checkout: source).split("\0").filter_map do |entry|
          next if entry.start_with?('160000 ')
          match = entry.match(/\A[0-9]+ ([0-9a-f]{40}|[0-9a-f]{64}) [0-3]\t/)
          match && match[1] unless match && match[1].match?(/\A0+\z/)
        end.uniq
        index_objects.each { |object| git(record, 'update-ref', "#{ref_prefix}/index/#{object}", object) }
        Dir.glob(File.join(destination, 'admin', '**', '*')).each do |path|
          next unless File.file?(path) && !File.symlink?(path) && File.size(path) <= 1024 * 1024
          next if File.basename(path).start_with?('index', 'sharedindex')

          File.binread(path).scan(/\b(?:[0-9a-f]{64}|[0-9a-f]{40})\b/).uniq.each do |object|
            _out, _err, status = @commands.capture(
              ['git', "--git-dir=#{record.fetch('common_dir')}", 'cat-file', '-e', object], allow_failure: true
            )
            git(record, 'update-ref', "#{ref_prefix}/operation/#{object}", object) if status.success?
          end
        end
        saved = record.reject { |name, _| name == 'removed' }.merge(
          'files_sha256' => tree_digest(File.join(destination, 'files')),
          'admin_sha256' => tree_digest(File.join(destination, 'admin')),
          'head_ref' => "#{ref_prefix}/head"
        )
        sync_tree(destination)
        write_json(File.join(destination, 'record.json'), saved)
        verify_checkout!(record)
      end
      write_json(File.join(@root, 'inventory.json'), { 'schema' => 1, 'worktrees' => records })
    end

    def records
      inventory = JSON.parse(File.binread(File.join(@root, 'inventory.json')))
      unless inventory['schema'] == 1 && inventory['worktrees'].is_a?(Array)
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
      saved, destination = read_record(record)
      source = File.join(@group, record.fetch('path'))
      unless tree_digest(source, excludes: ['.git']) == saved.fetch('files_sha256') &&
             tree_digest(record.fetch('admin_dir'), excludes: ADMIN_EXCLUDES) == saved.fetch('admin_sha256')
        raise Error, "worktree changed after archive recovery capture: #{source}"
      end
      verify_snapshot!(saved, destination)
    end

    def verify_snapshot!(saved, destination)
      unless tree_digest(File.join(destination, 'files')) == saved.fetch('files_sha256') &&
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
                      ['worktree', 'add', target, branch]
                    else
                      ['worktree', 'add', '--detach', target, saved.fetch('head_ref')]
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
      Dir.children(target).each { |name| FileUtils.rm_rf(File.join(target, name)) unless name == '.git' }
      copy_contents(File.join(destination, 'files'), target)
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
