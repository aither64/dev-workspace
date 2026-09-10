# frozen_string_literal: true

require 'digest'
require 'json'

module DevWorkspaceProfileIdentity
  def self.token(path)
    stat = File.lstat(path)
    return nil unless stat.symlink?

    identity = [
      stat.dev, stat.ino, stat.ctime.to_r, stat.mtime.to_r,
      File.readlink(path)
    ]
    Digest::SHA256.hexdigest(JSON.generate(identity))
  rescue Errno::ENOENT, Errno::EACCES, Errno::ELOOP
    nil
  end
end
