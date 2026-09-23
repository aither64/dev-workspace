# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class TeamMcpBindingTest < DevSessionTest
  class RecordingCapture
    attr_reader :calls

    def initialize
      @calls = []
    end

    def capture(argv, input: nil)
      @calls << [argv, input]
      ['', '', nil]
    end
  end

  class RecordingTeamRunner
    attr_reader :calls

    def initialize
      @calls = []
    end

    def resolve_slug(value, as_is:)
      value
    end

    def team(slug, action:, **options)
      @calls << [slug, action, options]
      ''
    end
  end

  def test_report_refuses_recreated_slug_with_a_different_lead_thread
    with_workspace do |workspace|
      slug = 'member-report'
      original_root = 'original-lead-thread'
      current_root = 'replacement-lead-thread'
      runner = team_runner(workspace, slug, original_root)
      prepare_team_manifest(runner, slug, current_root)

      error = assert_raises(DevSession::Error) do
        runner.team(slug, action: 'assign', from: 'reviewer0', to: 'lead',
                    message: 'Report text', message_id: 'a' * 32)
      end
      assert_includes(error.message, 'lead thread changed')

      current = team_runner(workspace, slug, current_root)
      assert_equal('', current.team(slug, action: 'assign', from: 'reviewer0', to: 'lead',
                                    message: 'Report text', message_id: 'a' * 32))
    end
  end

  def test_assign_message_uses_stdin_across_both_cli_hops
    with_workspace do |workspace|
      slug = 'member-report'
      runner = team_runner(workspace, slug, 'lead-thread')
      prepare_team_manifest(runner, slug, 'lead-thread')

      capture = RecordingCapture.new
      reporter = team_runner(workspace, slug, 'lead-thread', command_runner: capture)
      assert_equal('', reporter.team(slug, action: 'assign', from: 'reviewer0', to: 'lead',
                                     message: 'Private report text', message_id: 'b' * 32))
      assert_equal(1, capture.calls.length)
      argv, input = capture.calls.fetch(0)
      assert_equal('Private report text', input)
      assert_equal('/dev/stdin', argv.fetch(argv.index('--input-file') + 1))
      refute_includes(argv.join(' '), 'Private report text')
      refute_includes(argv, '--message')
    end
  end

  def test_message_stdin_is_bounded_and_exclusive_to_assign
    recorder = RecordingTeamRunner.new
    command = ['team', 'assign', 'member-report', '--as-is', '--to', 'lead',
               '--message-stdin', '--message-id', 'c' * 32]
    output = StringIO.new
    errors = StringIO.new
    cli = DevSession::CLI.new(command, input: StringIO.new('Private report text'), out: output, err: errors)
    cli.instance_variable_set(:@runner, recorder)
    assert_equal(0, cli.run)
    assert_equal('Private report text', recorder.calls.fetch(0).fetch(2).fetch(:message))

    conflicting = DevSession::CLI.new(command + ['--message', 'other'], input: StringIO.new('secret'),
                                      out: StringIO.new, err: StringIO.new)
    conflicting.instance_variable_set(:@runner, recorder)
    assert_equal(1, conflicting.run)
    oversized = DevSession::CLI.new(command, input: StringIO.new('x' * (DevSession::MAX_MESSAGE_BYTES + 1)),
                                    out: StringIO.new, err: StringIO.new)
    oversized.instance_variable_set(:@runner, recorder)
    assert_equal(1, oversized.run)
    assert_equal(1, recorder.calls.length)
  end

  private

  def team_runner(workspace, slug, expected_root, command_runner: nil)
    DevSession::Runner.new(workspace:, tmux: Object.new, portal_command: [RbConfig.ruby, '-e', ''],
                           command_runner:, out: StringIO.new, err: StringIO.new, env: {
      'XDG_STATE_HOME' => File.join(workspace, '.xdg-state'),
      DevSession::ENV_SLUG => slug,
      DevSession::ENV_MEMBER_ADDRESS => 'reviewer0',
      DevSession::ENV_EXPECTED_ROOT_THREAD_ID => expected_root
    })
  end

  def prepare_team_manifest(runner, slug, root_thread)
    runner.ensure_tracking_files(slug)
    manifest = runner.send(:new_portal_manifest, slug)
    manifest['codex']['thread_id'] = root_thread
    runner.send(:write_portal_manifest, slug, manifest)
  end
end
