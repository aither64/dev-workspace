# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  class MonitorRecorder
    attr_reader :calls
    def initialize
      @calls = []
    end
    def resolve_slug(value, as_is:)
      value
    end
    def monitor(slug, request)
      @calls << [slug, request]
      '{}'
    end
  end

  def test_cli_preserves_exact_operation_and_resolution_ids
    %w[start report status cancel resolve].each do |action|
      recorder = MonitorRecorder.new
      request = {'action' => action, 'id' => 'a' * 32, 'resolutionId' => 'b' * 32}
      cli = DevSession::CLI.new(['monitor', action, 'source', '--as-is', '--request-stdin'],
        input: StringIO.new(JSON.generate(request)), out: StringIO.new, err: StringIO.new)
      cli.instance_variable_set(:@runner, recorder)
      assert_equal 0, cli.run
      assert_equal [['source', request]], recorder.calls
    end
  end

  def test_cli_refuses_changed_action_and_unsupported_lifecycle_command
    %w[start archive].each do |action|
      recorder = MonitorRecorder.new
      cli = DevSession::CLI.new(['monitor', action, 'source', '--as-is', '--request-stdin'],
        input: StringIO.new('{"action":"report"}'), out: StringIO.new, err: StringIO.new)
      cli.instance_variable_set(:@runner, recorder)
      assert_equal 1, cli.run
      assert_empty recorder.calls
    end
  end

  def test_native_binding_refuses_member_or_foreign_session_before_host_dispatch
    with_workspace do |workspace|
      [{DevSession::ENV_MEMBER_ADDRESS => 'implementer0'}, {DevSession::ENV_SLUG => 'other'}].each do |env|
        runner = DevSession::Runner.new(workspace:, tmux: Object.new, portal_command: ['unused'],
          out: StringIO.new, err: StringIO.new, env: env.merge('XDG_STATE_HOME' => File.join(workspace, '.state')))
        assert_raises(DevSession::Error) { runner.monitor('source', {'action' => 'start'}) }
      end
    end
  end

  def test_solo_monitor_preflight_precedes_root_quiescence_and_stop_does_not_revoke
    runner = DevSession::Runner.allocate
    events = []
    runner.define_singleton_method(:sync_monitor_lifecycle!) { |slug, action| events << ['monitor', slug, action] }
    runner.define_singleton_method(:quiesce_native_client!) { |*| events << ['root']; false }
    runner.define_singleton_method(:ensure_portal_thread_idle!) { |*args, **options| events << ['idle', options] }
    runner.define_singleton_method(:sync_team_lifecycle!) { |*| }
    runner.send(:quiesce_and_require_idle!, 'solo', nil)
    assert_equal ['monitor', 'solo', 'require-idle'], events[0]
    assert_equal ['root'], events[1]
    refute events.any? { |item| item.include?('revoke') }
    events.clear
    runner.send(:quiesce_and_require_idle!, 'solo', nil, package_transition: true)
    assert_equal ['monitor', 'solo', 'require-execution-idle'], events[0]
    events.clear
    runner.define_singleton_method(:sync_monitor_lifecycle!) { |*| raise DevSession::Error, 'unknown utility launch' }
    assert_raises(DevSession::Error) { runner.send(:quiesce_and_require_idle!, 'solo', nil) }
    assert_empty events
  end
end
