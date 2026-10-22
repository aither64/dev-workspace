# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_retained_artifact_captions_load_without_rewriting_the_manifest
    with_workspace do |workspace|
      runner = artifact_registration_runner(workspace)
      path = retained_artifact_manifest(workspace)
      before = artifact_workspace_snapshot(workspace)

      parsed = runner.send(:load_portal_manifest, path, required: true)
      assert_equal(['Report', 'Canonical caption', 'Fallback caption', 'Unicode fallback caption'],
                   parsed['artifacts'].map { |artifact| artifact['label'] })
      assert_equal(before, artifact_workspace_snapshot(workspace))
    end
  end

  def test_deprecated_artifact_cli_preserves_existing_manifest_bytes_mtime_and_labels
    with_workspace do |workspace|
      retained_artifact_manifest(workspace)
      before = artifact_workspace_snapshot(workspace)
      output, errors, status = Open3.capture3(
        RbConfig.ruby, File.expand_path('../../libexec/dev-session', __dir__),
        '--workspace', workspace, 'artifact', 'add', '2026-09-03-example', './report.json',
        '--label', 'Updated report', '--as-is'
      )

      assert_equal(0, status.exitstatus, errors)
      assert_artifact_deprecation_notice(output)
      assert_empty(errors)
      assert_equal(before, artifact_workspace_snapshot(workspace))
    end
  end

  def test_deprecated_artifact_runner_does_not_initialize_an_omitted_artifact_list
    with_workspace do |workspace|
      output = StringIO.new
      runner = artifact_registration_runner(workspace, out: output)
      path = retained_artifact_manifest(workspace)
      manifest = YAML.safe_load(File.read(path))
      manifest.delete('artifacts')
      File.write(path, YAML.dump(manifest))
      before = artifact_workspace_snapshot(workspace)

      runner.artifact_add('2026-09-03-example', 'new.json', label: 'New report', as_is: true)

      assert_artifact_deprecation_notice(output.string)
      assert_equal(before, artifact_workspace_snapshot(workspace))
    end
  end

  def test_deprecated_artifact_runner_preserves_existing_entries_and_ignores_expected_owner
    with_workspace do |workspace|
      output = StringIO.new
      runner = artifact_registration_runner(workspace, out: output)
      retained_artifact_manifest(workspace)
      before = artifact_workspace_snapshot(workspace)

      runner.artifact_add('2026-09-03-example', './report.json', label: 'Updated report',
                         as_is: true, expected_owner: SecureRandom.uuid)
      runner.artifact_add('2026-09-03-example', 'new.json', label: 'New report',
                         as_is: true, expected_owner: 'retained-owner')

      assert_artifact_deprecation_notice(output.string, count: 2)
      assert_equal(before, artifact_workspace_snapshot(workspace))
    end
  end

  def test_deprecated_artifact_runner_does_not_create_missing_tracking_or_files
    with_workspace do |workspace|
      output = StringIO.new
      runner = artifact_registration_runner(workspace, out: output)
      before = artifact_workspace_snapshot(workspace)

      runner.artifact_add('missing', 'nested/report.json', label: 'Report', as_is: false)

      assert_artifact_deprecation_notice(output.string)
      assert_equal(before, artifact_workspace_snapshot(workspace))
      refute(File.exist?(File.join(workspace, 'work', 'missing')))
    end
  end

  def test_deprecated_artifact_runner_does_not_read_manifests_or_operation_journals
    with_workspace do |workspace|
      output = StringIO.new
      runner = artifact_registration_runner(workspace, out: output)
      path = retained_artifact_manifest(workspace)
      File.binwrite(path, "invalid manifest\0")
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-03-example.creation.json'), 'invalid receipt')
      File.write(File.join(locks, '2026-09-03-example.archive.json'), 'invalid journal')
      before = artifact_workspace_snapshot(workspace)

      runner.artifact_add('2026-09-03-example', 'report.json', label: 'Report', as_is: true)

      assert_artifact_deprecation_notice(output.string)
      assert_equal(before, artifact_workspace_snapshot(workspace))
    end
  end

  def test_deprecated_artifact_cli_needs_no_workspace_runtime_or_transition_lock
    with_workspace do |workspace|
      output, errors = StringIO.new, StringIO.new
      before = artifact_workspace_snapshot(workspace)
      cli = DevSession::CLI.new([
        '--workspace', File.join(workspace, 'missing-workspace'),
        '--transition-lock', File.join(workspace, 'missing-locks', 'transition.lock'),
        '--require-runtime', 'artifact', 'add', 'missing', 'nested/report.json',
        '--label', 'Report', '--expected-owner', SecureRandom.uuid, '--as-is'
      ], out: output, err: errors)

      assert_equal(0, cli.run, errors.string)
      assert_artifact_deprecation_notice(output.string)
      assert_empty(errors.string)
      assert_equal(before, artifact_workspace_snapshot(workspace))
    end
  end

  def test_deprecated_artifact_cli_accepts_an_omitted_label
    output, errors = StringIO.new, StringIO.new
    cli = DevSession::CLI.new(['artifact', 'add', 'example', 'report.json'],
                             out: output, err: errors)

    assert_equal(0, cli.run, errors.string)
    assert_artifact_deprecation_notice(output.string)
    assert_empty(errors.string)
  end

  def test_deprecated_artifact_cli_preserves_argument_and_option_errors
    [
      ['artifact'],
      ['artifact', 'remove', 'example', 'report.json'],
      ['artifact', 'add', 'example'],
      ['artifact', 'add', 'example', 'report.json', 'extra'],
      ['artifact', 'add', 'example', 'report.json', '--unknown'],
      ['artifact', 'add', 'example', 'report.json', '--label'],
      ['artifact', 'add', 'example', 'report.json', '--expected-owner']
    ].each do |arguments|
      output, errors = StringIO.new, StringIO.new
      cli = DevSession::CLI.new(arguments, out: output, err: errors)

      assert_equal(1, cli.run, arguments.inspect)
      assert_empty(output.string)
      assert_match(/\Aerror: /, errors.string)
    end
  end

  def test_retained_artifact_manifest_validation_still_rejects_an_escape
    with_workspace do |workspace|
      runner = artifact_registration_runner(workspace)
      path = retained_artifact_manifest(workspace)
      manifest = YAML.safe_load(File.read(path))
      manifest['artifacts'].first['path'] = '../outside.json'
      File.write(path, YAML.dump(manifest))
      before = artifact_workspace_snapshot(workspace)

      assert_raises(DevSession::Error) { runner.send(:load_portal_manifest, path, required: true) }
      assert_equal(before, artifact_workspace_snapshot(workspace))
    end
  end

  private

  def artifact_registration_runner(workspace, out: StringIO.new)
    DevSession::Runner.new(
      workspace:, tmux: Object.new, out:, err: StringIO.new,
      today: Date.new(2026, 9, 3), cwd: workspace,
      env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') },
      cluster_providers: {}
    )
  end

  def retained_artifact_manifest(workspace)
    path = File.join(workspace, 'work', '2026-09-03-example', 'portal.yml')
    FileUtils.mkdir_p(File.dirname(path))
    FileUtils.cp(File.expand_path('../fixtures/portal-manifest-valid-artifact-names.yml', __dir__), path)
    File.utime(Time.at(1_700_000_000), Time.at(1_700_000_000), path)
    path
  end

  def artifact_workspace_snapshot(workspace)
    Dir.glob('**/*', File::FNM_DOTMATCH, base: workspace).sort.to_h do |relative|
      path = File.join(workspace, relative)
      stat = File.lstat(path)
      [relative, [stat.ftype, stat.mode, stat.mtime, stat.file? ? File.binread(path) : nil]]
    end
  end

  def assert_artifact_deprecation_notice(output, count: 1)
    notice = "File registration is no longer needed. Session files appear automatically in Files.\n"
    assert_equal(notice * count, output)
  end
end
