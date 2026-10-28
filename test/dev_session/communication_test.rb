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
end
