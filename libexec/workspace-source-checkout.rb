# frozen_string_literal: true

# Tracking is deliberately absent from additional workspace checkouts. The
# shared checkout and every independent project retain their ordinary layout.
module WorkspaceSourceCheckout
  PATTERNS = "/*\n!/work/\n!/archive/\n".freeze

  def self.workspace?(common, workspace)
    File.directory?(File.join(workspace, '.git')) &&
      File.realpath(common) == File.realpath(File.join(workspace, '.git'))
  end

  def self.configure(path, commands, initialize_index: false)
    commands.call(['git', '-C', path, 'sparse-checkout', 'set', '--no-cone', '--stdin'], input: PATTERNS)
    # Re-reading HEAD would reset staged entries in an existing checkout.
    commands.call(['git', '-C', path, 'read-tree', '-mu', 'HEAD']) if initialize_index
  end
end
