# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_ruby_manifest_validator_accepts_shared_fixture
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      directory = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(directory)
      FileUtils.cp(
        File.expand_path('../fixtures/portal-manifest-valid.yml', __dir__),
        File.join(directory, 'portal.yml')
      )

      manifest = runner_for(workspace).send(
        :load_portal_manifest,
        File.join(directory, 'portal.yml'),
        required: true
      )
      assert_equal(slug, manifest['slug'])
      assert_equal('ready', manifest.dig('creation', 'state'))
      refute(manifest.key?('tmux'))
      assert_equal('2026-09-03T12:00:00Z', manifest['finalized_at'])
    end
  end

  def test_ruby_manifest_validator_accepts_all_shared_valid_fixtures
    fixtures = Dir[File.expand_path('../fixtures/portal-manifest-valid*.yml', __dir__)]
    assert_operator(fixtures.length, :>=, 2)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        slug = '2026-09-03-example'
        directory = File.join(workspace, 'work', slug)
        FileUtils.mkdir_p(directory)
        FileUtils.cp(fixture, File.join(directory, 'portal.yml'))

        manifest = runner_for(workspace).send(
          :load_portal_manifest,
          File.join(directory, 'portal.yml'),
          required: true
        )
        assert_equal(slug, manifest['slug'], File.basename(fixture))
      end
    end
  end

  def test_validate_checks_all_persisted_portal_manifests
    with_workspace do |workspace|
      valid_directory = File.join(workspace, 'work', '2026-09-03-example')
      invalid_directory = File.join(workspace, 'archive', '2026-09-03-invalid')
      FileUtils.mkdir_p(valid_directory)
      FileUtils.mkdir_p(invalid_directory)
      FileUtils.cp(
        File.expand_path('../fixtures/portal-manifest-valid.yml', __dir__),
        File.join(valid_directory, 'portal.yml')
      )
      File.write(
        File.join(invalid_directory, 'portal.yml'),
        "schema: 1\nslug: 2026-09-03-other\n"
      )

      assert_raises(DevSession::Error) do
        runner_for(workspace).validate
      end
    end
  end

  def test_validate_enforces_tracking_lifecycle_and_duplicate_placement
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      File.write(
        File.join(workspace, 'work', slug, 'portal.yml'),
        "schema: 1\nslug: #{slug}\nrepositories: []\nartifacts: []\n"
      )
      out = StringIO.new
      validating_runner = runner_for(workspace, out:)
      validating_runner.validate
      assert_includes(out.string, 'validated 1 portal manifest')

      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Plan\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n"
      )
      File.write(
        File.join(archive, 'portal.yml'),
        "schema: 1\nslug: #{slug}\nfinalized_at: '2026-09-03T12:00:00Z'\nrepositories: []\nartifacts: []\n"
      )
      error = assert_raises(DevSession::Error) do
        validating_runner.validate
      end
      assert_includes(error.message, 'duplicate session')

      FileUtils.rm_rf(File.join(workspace, 'work', slug))
      File.write(File.join(archive, 'state.md'), "---\nlifecycle: active\n---\n")
      error = assert_raises(DevSession::Error) do
        validating_runner.validate
      end
      assert_includes(error.message, 'terminal lifecycle')
    end
  end

  def test_ruby_manifest_validator_rejects_shared_invalid_fixtures
    fixtures = Dir[File.expand_path('../fixtures/portal-manifest-invalid-*.yml', __dir__)]
    refute_empty(fixtures)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        slug = '2026-09-03-example'
        directory = File.join(workspace, 'work', slug)
        FileUtils.mkdir_p(directory)
        FileUtils.cp(fixture, File.join(directory, 'portal.yml'))

        assert_raises(DevSession::Error, File.basename(fixture)) do
          runner_for(workspace).send(
            :load_portal_manifest,
            File.join(directory, 'portal.yml'),
            required: true
          )
        end
      end
    end
  end

  def test_ruby_runtime_authority_validator_uses_shared_corpus
    runner = runner_for('/tmp')
    Dir[File.expand_path('../fixtures/runtime-authority-valid-*.json', __dir__)].each do |fixture|
      record = JSON.parse(File.read(fixture))
      record['workspace'] = runner.workspace
      runner.send(:validate_session_authority!, record, 'example', fixture)
    end
    Dir[File.expand_path('../fixtures/runtime-authority-invalid-*.json', __dir__)].each do |fixture|
      record = JSON.parse(File.read(fixture))
      record['workspace'] = runner.workspace
      assert_raises(DevSession::Error, File.basename(fixture)) do
        runner.send(:validate_session_authority!, record, 'example', fixture)
      end
    end
  end

  def test_exclusive_start_refuses_to_reuse_an_existing_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          exclusive: true
        )
      end

      assert_match(/already exists/, error.message)
    end
  end

  def test_tracking_files_refuse_an_archived_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'archive', slug))

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_a_dangling_archive_symlink
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'archive'))
      FileUtils.ln_s(
        File.join(workspace, 'missing-archive-target'),
        File.join(workspace, 'archive', slug)
      )

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_a_slug_found_only_in_archive_history
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Plan\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n\n# #{slug}\n\n## Status\n"
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('archive', slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'archive initiative')
      FileUtils.rm_r(archive)
      assert_git_success('git', '-C', workspace, 'add', '-A', '--', File.join('archive', slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'remove archive checkout')
      assert_equal(
        '',
        git_capture_success('git', '-C', workspace, 'ls-files', '--', File.join('archive', slug))
      )

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_a_slug_found_only_in_the_git_index
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Plan\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n\n# #{slug}\n\n## Status\n"
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('archive', slug))
      FileUtils.rm_r(archive)
      refute_empty(
        git_capture_success('git', '-C', workspace, 'ls-files', '--', File.join('archive', slug))
      )

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_file_creation_does_not_execute_workspace_git_configuration
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      assert_git_success('git', 'init', '-b', 'master', workspace)
      marker = File.join(workspace, 'git-hook-ran')
      hook = File.join(workspace, 'hostile-fsmonitor')
      File.write(hook, "#!/bin/sh\ntouch #{Shellwords.escape(marker)}\nprintf '{}\\n'\n")
      FileUtils.chmod(0o755, hook)
      assert_git_success('git', '-C', workspace, 'config', 'core.fsmonitor', hook)

      runner_for(workspace).ensure_tracking_files(slug)

      refute(File.exist?(marker))
      assert(File.file?(File.join(workspace, 'work', slug, 'plan.md')))
    end
  end

  def test_tracking_files_fail_closed_when_archive_history_cannot_be_read
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      File.write(File.join(workspace, '.git'), "not a git directory\n")

      assert_raises(DevSession::CommandError) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tracking_files_refuse_an_existing_empty_partial_write
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      directory = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(directory)
      File.write(File.join(directory, 'plan.md'), '')

      error = assert_raises(DevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/empty or truncated/, error.message)
    end
  end

  def test_list_output_uses_aligned_columns_for_long_slugs
    with_workspace do |workspace|
      short_slug = '2026-06-06-short'
      long_slug = '2026-06-06-this-is-a-longer-development-session-name'

      FileUtils.mkdir_p(File.join(workspace, 'work', short_slug))
      FileUtils.mkdir_p(File.join(workspace, 'work', long_slug))

      worktree = File.join(workspace, 'worktrees', long_slug, 'alpha')
      FileUtils.mkdir_p(worktree)
      File.write(File.join(worktree, '.git'), "gitdir: /tmp/nonexistent\n")

      out = StringIO.new
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY
      )

      runner.list

      lines = out.string.lines.map(&:chomp)
      header = lines.fetch(0)
      work_column = header.index('WORK')
      worktrees_column = header.index('WORKTREES')
      tmux_column = header.index('TMUX')

      assert_equal(3, lines.length)

      lines.drop(1).each do |line|
        assert_equal('yes', line[work_column, 3])
        assert_match(/[0-9]/, line[worktrees_column, 'WORKTREES'.length])
        assert_equal('none', line[tmux_column, 4])
      end
    end
  end

  def test_current_uses_environment_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      runner = runner_for(
        workspace,
        env: {
          DevSession::ENV_SLUG => slug,
          DevSession::ENV_WORKSPACE => workspace
        },
        out:
      )

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_rejects_an_environment_from_another_workspace
    with_workspace do |workspace|
      Dir.mktmpdir('foreign-dev-session-workspace') do |foreign_workspace|
        runner = runner_for(
          workspace,
          env: {
            DevSession::ENV_SLUG => '2026-06-06-demo',
            DevSession::ENV_WORKSPACE => foreign_workspace
          }
        )

        error = assert_raises(DevSession::Error) { runner.current }

        assert_match(/not managed by this workspace/, error.message)
      end
    end
  end

  def test_current_uses_managed_tmux_session_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      runner = runner_for(
        workspace,
        tmux: CurrentTmux.new(slug, workspace:),
        env: { 'TMUX' => 'socket', 'TMUX_PANE' => '%1' },
        out:
      )

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_rejects_a_tmux_session_from_another_workspace
    with_workspace do |workspace|
      Dir.mktmpdir('foreign-dev-session-workspace') do |foreign_workspace|
        tmux = CurrentTmux.new('2026-06-06-demo', workspace: foreign_workspace)
        runner = runner_for(
          workspace,
          tmux:,
          env: { 'TMUX' => 'socket', 'TMUX_PANE' => '%1' }
        )

        error = assert_raises(DevSession::Error) { runner.current }

        assert_match(/not managed by this workspace/, error.message)
      end
    end
  end

  def test_current_uses_work_directory_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cwd = File.join(workspace, 'work', slug, 'notes')
      FileUtils.mkdir_p(cwd)

      runner = runner_for(workspace, cwd:)

      assert_equal(slug, runner.current_slug)
    end
  end

  def test_current_uses_worktrees_directory_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cwd = File.join(workspace, 'worktrees', slug, 'alpha', 'app')
      FileUtils.mkdir_p(cwd)

      runner = runner_for(workspace, cwd:)

      assert_equal(slug, runner.current_slug)
    end
  end

  def test_current_reports_missing_active_session
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(DevSession::Error) do
        runner.current
      end

      assert_match(/no current dev session/, error.message)
    end
  end

  def test_current_ignores_tmux_server_current_session_outside_a_tmux_pane
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        cwd: workspace
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      error = assert_raises(DevSession::Error) { runner.current }

      assert_match(/no current dev session/, error.message)
      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_current_reports_conflicting_sources
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      other_slug = '2026-06-06-other'
      cwd = File.join(workspace, 'work', other_slug)
      FileUtils.mkdir_p(cwd)

      runner = runner_for(
        workspace,
        env: {
          DevSession::ENV_SLUG => slug,
          DevSession::ENV_WORKSPACE => workspace
        },
        cwd:
      )

      error = assert_raises(DevSession::Error) do
        runner.current
      end

      assert_match(/sources disagree/, error.message)
      assert_match(/#{DevSession::ENV_SLUG}=#{slug}/, error.message)
      assert_match(/cwd=#{other_slug}/, error.message)
    end
  end

end
