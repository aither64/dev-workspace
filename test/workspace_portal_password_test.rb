# frozen_string_literal: true

require 'fileutils'
require 'minitest/autorun'
require 'open3'
require 'tmpdir'

class WorkspacePortalPasswordTest < Minitest::Test
  SCRIPT = File.expand_path('../bin/workspace-portal-password-hash', __dir__)
  PASSWORD = '0123456789abcdef' * 4

  def setup
    skip 'htpasswd is not available' unless command_available?('htpasswd')
  end

  def test_hash_is_repeatable_and_verifies_with_htpasswd
    Dir.mktmpdir('workspace-portal-password-test') do |directory|
      password_file = write_password(directory, PASSWORD)
      first = run_success(password_file)
      second = run_success(password_file)

      assert_match(/\Aaither:\$2[aby]\$12\$[.\/A-Za-z0-9]{53}\n\z/, first)
      assert_match(/\Aaither:\$2[aby]\$12\$[.\/A-Za-z0-9]{53}\n\z/, second)
      refute_equal(first, second)
      verify_hash(directory, first)
      verify_hash(directory, second)
    end
  end

  def test_rejects_malformed_permissive_and_symlinked_password_files
    Dir.mktmpdir('workspace-portal-password-test') do |directory|
      malformed = write_password(directory, 'not-a-password')
      assert_failure(malformed, '64-character lowercase hex')

      permissive = write_password(directory, PASSWORD)
      File.chmod(0o644, permissive)
      assert_failure(permissive, 'mode 0600')

      target = write_password(directory, PASSWORD)
      linked = File.join(directory, 'linked-password')
      File.symlink(target, linked)
      assert_failure(linked, 'regular, non-symlink')
    end
  end

  private

  def write_password(directory, value)
    path = File.join(directory, "password-#{rand(1_000_000)}")
    File.write(path, "#{value}\n")
    File.chmod(0o600, path)
    path
  end

  def run_success(path)
    stdout, stderr, status = Open3.capture3('bash', SCRIPT, path)
    assert(status.success?, stderr)
    stdout
  end

  def assert_failure(path, message)
    _stdout, stderr, status = Open3.capture3('bash', SCRIPT, path)
    refute(status.success?)
    assert_includes(stderr, message)
  end

  def verify_hash(directory, hash)
    auth_file = File.join(directory, "htpasswd-#{rand(1_000_000)}")
    File.write(auth_file, hash)
    _stdout, stderr, status = Open3.capture3(
      'htpasswd', '-vb', auth_file, 'aither', PASSWORD
    )
    assert(status.success?, stderr)
  end

  def command_available?(name)
    ENV.fetch('PATH', '').split(File::PATH_SEPARATOR).any? do |directory|
      File.executable?(File.join(directory, name))
    end
  end
end
