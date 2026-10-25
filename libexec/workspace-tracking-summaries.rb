# frozen_string_literal: true

# Tracking contains human explanations; raw verification output is disposable.
# Used before sealing a fresh archive and by the explicit maintenance command.
module WorkspaceTrackingSummaries
  def self.keep?(relative)
    File.extname(relative).downcase == '.md' || File.basename(relative) == 'portal.yml'
  end

  def self.prune(root, apply: false)
    removed = []
    walk = lambda do |directory|
      Dir.children(directory).sort.each do |name|
        path = File.join(directory, name)
        relative = path.delete_prefix(root + '/')
        if File.directory?(path) && !File.symlink?(path)
          walk.call(path)
          Dir.rmdir(path) if apply && Dir.empty?(path)
        elsif !keep?(relative)
          removed << path
          File.unlink(path) if apply
        end
      end
    end
    walk.call(root)
    manifest = File.join(root, 'portal.yml')
    if apply && File.file?(manifest) && !File.symlink?(manifest)
      value = YAML.safe_load(File.binread(manifest), permitted_classes: [], aliases: false)
      if value.is_a?(Hash) && value['artifacts'].is_a?(Array)
        value['artifacts'].select! { |item| keep?(item.fetch('path')) }
        bytes = YAML.dump(value)
        File.write(manifest, bytes) unless File.binread(manifest) == bytes
      end
    end
    removed
  end
end
