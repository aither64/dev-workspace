# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class CommunicationTest < DevSessionTest
  class Recorder
    attr_reader :calls
    def initialize
      @calls = []
    end
    def resolve_slug(value, as_is:)
      value
    end
    def communication(slug, request)
      @calls << [slug, request]
      '{}'
    end
  end

  def test_escaped_message_survives_cli_transport_and_retains_retry_id
    recorder = Recorder.new
    request = { 'action' => 'send', 'peer' => 'peer', 'message' => "\t" * 20_000, 'messageId' => 'a' * 32 }
    cli = DevSession::CLI.new(['communication', 'send', 'source', '--as-is', '--request-stdin'],
      input: StringIO.new(JSON.generate(request)), out: StringIO.new, err: StringIO.new)
    cli.instance_variable_set(:@runner, recorder)
    assert_equal 0, cli.run
    assert_equal [['source', request]], recorder.calls
  end

  def test_member_and_foreign_source_are_refused
    with_workspace do |workspace|
      [{DevSession::ENV_MEMBER_ADDRESS => 'implementer0'}, {DevSession::ENV_SLUG => 'other'}].each do |env|
        runner = DevSession::Runner.new(workspace:, tmux: Object.new, portal_command: ['unused'],
          out: StringIO.new, err: StringIO.new, env: env.merge('XDG_STATE_HOME' => File.join(workspace, '.state')))
        assert_raises(DevSession::Error) { runner.communication('source', {'action' => 'allow', 'peer' => 'peer'}) }
      end
    end
  end

  def test_communication_invocation_inherits_the_held_file_and_expires_after_failure
    with_workspace do |workspace|
      package = File.join(workspace, 'package')
      profile = File.join(workspace, 'profile')
      path = File.join(workspace, 'transition.lock')
      FileUtils.mkdir_p(package)
      File.symlink(package, profile)
      helper = File.join(workspace, 'borrowed-communication.rb')
      File.write(helper, <<~'RUBY')
        require 'json'
        def value(name)
          ARGV.fetch(ARGV.index(name) + 1)
        end
        borrowed = File.for_fd(Integer(value('--transition-lock-fd')), autoclose: false)
        expected = File.stat(value('--transition-lock'))
        abort 'wrong descriptor' unless borrowed.stat.ino == expected.ino && borrowed.stat.dev == expected.dev
        probe = File.open(value('--transition-lock'), File::RDWR)
        abort 'parent SH missing' if probe.flock(File::LOCK_EX | File::LOCK_NB)
        identity = JSON.parse(value('--expected-profile-identity'))
        abort 'wrong raw tuple' unless identity.length == 7 && identity.last == value('--package-root')
        abort 'fixture child failed' if JSON.parse(STDIN.read)['fail']
        puts JSON.generate('fd' => borrowed.fileno, 'identity' => identity)
      RUBY
      owner = File.open(path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_SH)
      invocation = DevSession::CommunicationInvocation.new(file: owner, path:, package:, profile:)
      runner = DevSession::Runner.new(workspace:, tmux: Object.new,
        portal_command: [RbConfig.ruby, helper], out: StringIO.new, err: StringIO.new,
		authority_dir: File.join(workspace, 'authority'), codex_socket: File.join(workspace, 'codex.sock'),
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.state') })
      runner.with_communication_invocation(invocation) do
        result = JSON.parse(runner.communication('source', {'action' => 'list'}))
        assert_equal owner.fileno, result.fetch('fd')
        assert_equal DevSession::CommunicationInvocation.profile_identity(profile), result.fetch('identity')
        assert_raises(DevSession::Error) { runner.with_communication_invocation(invocation) { flunk } }
      end
      assert_nil runner.instance_variable_get(:@communication_invocation)
      assert_raises(DevSession::CommandError) do
        runner.with_communication_invocation(invocation) { runner.communication('source', {'fail' => true}) }
      end
      assert_nil runner.instance_variable_get(:@communication_invocation)
      refute owner.closed?
      probe = File.open(path, File::RDWR)
      refute probe.flock(File::LOCK_EX | File::LOCK_NB), 'child failure changed the parent SH'
      File.unlink(profile)
      File.symlink(package, profile)
      assert_raises(DevSession::Error) { runner.with_communication_invocation(invocation) { flunk } }
      assert_nil runner.instance_variable_get(:@communication_invocation)
    ensure
      probe&.close
      owner&.close
    end
  end

  def test_borrowed_transition_arguments_refuse_outside_a_complete_lifecycle_invocation
    [ ['--transition-lock-fd', '3', '--', 'communication', 'list', 'source'],
      ['--transition-lock-fd', 'bad', '--', 'archive', 'source'] ].each do |args|
      error = StringIO.new
      cli = DevSession::CLI.new(args, out: StringIO.new, err: error)
      assert_equal 1, cli.run
      assert_includes error.string, 'complete lifecycle invocation'
    end
  end
end
