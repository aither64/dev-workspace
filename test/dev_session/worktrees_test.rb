# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_worktree_add_and_remove_keep_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      path = File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
      assert(File.exist?(File.join(path, '.git')))

      runner.worktree_remove('demo', 'sample', as_is: false, force: false)

      refute(File.exist?(path))
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )
    end
  end

  def test_worktree_add_records_github_comparison_metadata
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git',
        "--git-dir=#{repository}",
        'remote',
        'set-url',
        'origin',
        'git@github.com:example-org/sample.git'
      )

      runner_for(workspace).worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', '2026-06-06-demo', 'portal.yml'))
      )
      metadata = manifest.fetch('repositories').fetch(0)
      assert_equal('sample', metadata['name'])
      assert_equal('sample', metadata['project'])
      assert_equal('example-org/sample', metadata['github'])
      assert_equal('2026-06-06-demo', metadata['branch'])
      assert_equal('master', metadata['default_branch'])
      assert_match(/\A[0-9a-f]{40}\z/, metadata['initial_base_sha'])
    end
  end

  def test_worktree_add_uses_one_immutable_base_and_recovers_registration
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      base_sha = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', '-b', slug, path, base_sha
      )

      runner.worktree_add(
        slug,
        'sample',
        as_is: true,
        name: nil,
        branch: slug,
        base: 'master',
        fetch: false
      )

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      metadata = manifest.fetch('repositories').fetch(0)
      assert_equal(base_sha, metadata['initial_base_sha'])
      assert_equal(slug, metadata['branch'])
    end
  end

  def test_explicit_base_does_not_change_recorded_default_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success('git', "--git-dir=#{repository}", 'branch', 'release', 'master')

      runner_for(workspace).worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'release',
        fetch: false
      )

      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', '2026-06-06-demo', 'portal.yml'))
      )
      assert_equal('master', manifest.dig('repositories', 0, 'default_branch'))
    end
  end

  def test_workspace_repository_worktree_can_be_finalized_with_stable_identity
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      File.write(File.join(workspace, 'README.md'), "# Workspace\n")
      assert_git_success('git', '-C', workspace, 'add', 'README.md')
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'initial workspace')
      remote = File.join(workspace, 'workspace-origin.git')
      assert_git_success('git', 'init', '--bare', remote)
      assert_git_success(
        'git', '-C', workspace, 'remote', 'add', 'origin',
        'git@github.com:example-org/example-workspace-workspace.git'
      )
      assert_git_success(
        'git', '-C', workspace, 'config',
        "url.#{remote}.insteadOf", 'git@github.com:example-org/example-workspace-workspace.git'
      )
      assert_git_success('git', '-C', workspace, 'push', 'origin', 'master')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug,
        'workspace',
        as_is: true,
        name: nil,
        branch: slug,
        base: 'master',
        fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'workspace')
      assert(File.exist?(File.join(path, '.git')))

      merge_registered_branches(workspace, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)

      refute(File.exist?(path))
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'archive', slug, 'portal.yml'))
      )
      repository = manifest.fetch('repositories').fetch(0)
      assert_equal('workspace', repository['name'])
      assert_equal('example-org/example-workspace-workspace', repository['github'])
      assert_match(/\A[0-9a-f]{40}\z/, repository['final_head_sha'])
      assert_git_success(
        'git', '-C', workspace, 'show-ref', '--verify', '--quiet',
        "refs/heads/#{slug}"
      )
    end
  end

  def test_worktree_add_accepts_an_in_root_repository_alias
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample-storage')
      FileUtils.ln_s(
        'sample-storage.git',
        File.join(workspace, 'repos', 'sample.git')
      )

      runner_for(workspace).worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      assert(
        File.directory?(
          File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
        )
      )
    end
  end

  def test_worktree_add_refuses_an_external_repository_alias
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      Dir.mktmpdir('external-dev-session-repository') do |external|
        FileUtils.mkdir_p(File.join(external, 'repos'))
        create_bare_repo(external, 'sample')
        FileUtils.ln_s(
          File.join(external, 'repos', 'sample.git'),
          File.join(workspace, 'repos', 'sample.git')
        )
        commands = []
        command_runner = CallbackCommandRunner.new(
          out: StringIO.new,
          err: StringIO.new
        ) { |argv| commands << argv }
        runner = DevSession::Runner.new(
          workspace:,
          command_runner:,
          tmux: NullTmux.new,
          out: StringIO.new,
          err: StringIO.new,
          today: TODAY
        )

        error = assert_raises(DevSession::Error) do
          runner.worktree_add(
            'demo',
            'sample',
            as_is: false,
            name: nil,
            branch: nil,
            base: 'master',
            fetch: true
          )
        end

        assert_match(/outside the canonical repository root/, error.message)
        refute(commands.any? { |argv| argv.include?('fetch') })
        refute(
          File.exist?(
            File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
          )
        )
        refute_git_success(
          'git',
          "--git-dir=#{File.join(external, 'repos', 'sample.git')}",
          'show-ref',
          '--verify',
          '--quiet',
          'refs/heads/2026-06-06-demo'
        )
      end
    end
  end

end
