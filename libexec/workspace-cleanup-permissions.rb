# frozen_string_literal: true

# Callers establish session ownership before granting temporary deletion access.
module WorkspaceCleanupPermissions
  class Error < StandardError; end

  def self.removable_directory?(path)
    stat = File.lstat(path)
    stat.directory? && File.readable?(path) && File.executable?(path) &&
      (File.writable?(path) || stat.owned?)
  end

  def self.each_directory(root)
    pending = [root]
    until pending.empty?
      directory = pending.pop
      raise Error, "cleanup directory changed: #{directory}" unless File.lstat(directory).directory?
      next if yield(directory) == false

      Dir.each_child(directory) do |name|
        child = File.join(directory, name)
        pending << child if File.lstat(child).directory?
      end
    end
  end

  def self.with_writable_directories(paths, recursive: false)
    changed = {}
    prepare = lambda do |path|
      stat = File.lstat(path)
      unless removable_directory?(path)
        raise Error, "archive cannot obtain directory deletion access: #{path}"
      end
      next if File.writable?(path)

      changed[path] ||= stat
      File.chmod((stat.mode & 0o7777) | 0o200, path)
    end
    paths.each do |path|
      if recursive
        each_directory(path, &prepare)
      else
        prepare.call(path)
      end
    end
    yield
  rescue SystemCallError => e
    raise Error, "archive cleanup failed: #{e.message}"
  ensure
    changed.to_a.reverse_each do |path, original|
      begin
        current = File.lstat(path)
      rescue Errno::ENOENT
        next
      end
      unless current.directory? && [current.dev, current.ino] == [original.dev, original.ino]
        raise Error, "cleanup directory changed before permission restoration: #{path}"
      end
      mode = original.mode & 0o7777
      File.chmod(mode, path) unless (current.mode & 0o7777) == mode
    rescue SystemCallError => e
      raise Error, "archive permission restoration failed: #{e.message}"
    end
  end
end
