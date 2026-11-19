# frozen_string_literal: true

require_relative '../support/dev_session_test_case'
require_relative '../support/pi_creation_fixture'

class DevSessionTest < Minitest::Test
  include PiCreationFixture

  def test_pi_worktrees_preserve_creation_and_register_capture_remove_without_tmux
    [false, true].each do |fork|
      with_workspace do |workspace|
        create_bare_repo(workspace, 'sample')
        runner, records, paths, authority_path = pi_worktree_fixture(workspace, fork:)
        slug = records[:intent]['slug']
        before = paths.to_h { |key, path| [key, File.binread(path)] }
        manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
        original_manifest = YAML.safe_load(File.read(manifest_path))
        runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
        path = File.join(workspace, 'worktrees', slug, 'sample')
        manifest = YAML.safe_load(File.read(manifest_path))
        base = manifest['repositories'][0]['initial_base_sha']
        assert_match(/\A[0-9a-f]{40}\z/, base)
        assert_equal(original_manifest, manifest.reject { |key, _| key == 'repositories' }.merge('repositories' => []))
        runner.worktree_capture_comparison(slug, 'sample', as_is: true, base: base, head: base)
        captured = JSON.parse(File.read(File.join(workspace, 'captured-args.json')))
        assert_equal('capture-comparison', captured[0])
        assert_includes(captured, slug)
        assert_equal(base, captured[captured.index('--head') + 1])
        File.write(File.join(path, 'dirty.txt'), 'preserve dirty data')
        assert_raises(DevSession::Error) { runner.worktree_remove(slug, 'sample', as_is: true, force: false) }
        File.unlink(File.join(path, 'dirty.txt'))
        runner.worktree_remove(slug, 'sample', as_is: true, force: false)
        refute_path_exists(path)
        repository = File.join(workspace, 'repos/sample.git')
        assert_git_success('git', "--git-dir=#{repository}", 'show-ref', '--verify', 'refs/heads/' + slug)
        manifest = YAML.safe_load(File.read(manifest_path))
        assert_equal(base, manifest['repositories'][0]['final_head_sha'])
        # Cache retirement and a later current binding/generation remain valid.
        File.unlink(paths[:receipt])
        records[:binding]['revision'] += 1
        File.write(paths[:binding], JSON.generate(records[:binding]))
        authority = JSON.parse(File.read(authority_path))
        authority['binding_revision'] += 1
        authority['generation'] = '7' * 64
        File.write(authority_path, JSON.generate(authority))
        runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: nil, fetch: false)
        assert_equal(base, YAML.safe_load(File.read(manifest_path))['repositories'][0]['initial_base_sha'])
        runner.worktree_remove(slug, 'sample', as_is: true, force: false)
        before.each { |key, bytes| assert_equal(bytes, File.binread(paths[key])) unless [:receipt, :binding].include?(key) }
        assert_equal(original_manifest['backend'], YAML.safe_load(File.read(manifest_path))['backend'])
      end
    end
  end

  def test_pi_worktree_admission_refuses_incomplete_or_replaced_ownership_before_git
    %w[completion companion manifest tracking incarnation inode authority lifecycle waiting].each do |fault|
      with_workspace do |workspace|
        create_bare_repo(workspace, 'sample')
        runner, records, paths, authority_path = pi_worktree_fixture(workspace)
        slug = records[:intent]['slug']
        case fault
        when 'completion', 'companion' then File.unlink(paths[fault == 'completion' ? :completion : :intent])
        when 'manifest' then File.unlink(File.join(workspace, 'work', slug, 'portal.yml'))
        when 'tracking' then File.write(File.join(workspace, 'work', slug, 'state.md'), "---\nlifecycle: archived\n---\n")
        when 'incarnation'
          binding = records[:binding].merge('session_incarnation' => 'f' * 64)
          File.write(paths[:binding], JSON.generate(binding))
        when 'inode'
          completion = records[:completion].merge('trackingInode' => records[:completion]['trackingInode'] + 1)
          File.write(paths[:completion], JSON.generate(completion))
        when 'authority' then File.write(authority_path, JSON.generate(JSON.parse(File.read(authority_path)).merge('state' => 'starting')))
        when 'lifecycle'
          path = runner.send(:lifecycle_journal_file, slug, 'archive')
          FileUtils.mkdir_p(File.dirname(path)); File.write(path, '{}')
        when 'waiting'
          original = runner.method(:with_host_lock)
          runner.define_singleton_method(:with_host_lock) do |*args, **options, &block|
            changed = records[:binding].merge('session_incarnation' => 'f' * 64)
            File.write(paths[:binding], JSON.generate(changed))
            original.call(*args, **options, &block)
          end
        end
        assert_raises(DevSession::Error, fault) { runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false) }
        refute_path_exists(File.join(workspace, 'worktrees', slug, 'sample'))
        refute(Open3.capture3('git', "--git-dir=#{File.join(workspace, 'repos/sample.git')}", 'show-ref', '--verify', 'refs/heads/' + slug).last.success?)
      end
    end
  end

  def pi_worktree_fixture(workspace, fork: false)
    root, authority = File.join(workspace, '.pi-state'), File.join(workspace, '.pi-authority')
    portal = File.join(workspace, 'capture.rb')
    File.write(portal, "require 'json'; File.write(#{File.join(workspace, 'captured-args.json').inspect}, JSON.generate(ARGV)); puts '{}'\n")
    tmux = NullTmux.new
    tmux.define_singleton_method(:session) { |_slug| raise 'Pi must never query or adopt tmux' }
    runner = runner_for(workspace, tmux:, authority_dir: authority, env: {'DEV_WORKSPACES_STATE' => root, DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, portal].shelljoin})
    records = fork ? pi_fork_join_records(workspace) : pi_creation_records(workspace)
    slug = records[:intent]['slug']
    runner.ensure_tracking_files(slug)
    manifest = {'schema' => fork ? 5 : 4, 'slug' => slug, 'backend' => {'kind' => 'pi', 'contract' => 1, 'session_incarnation' => 'a' * 64},
      'creation' => {'state' => 'ready'}, 'repositories' => []}
    if fork
      manifest['forked_from'] = records[:intent]['fork']['source']['Slug']
    else
      manifest['creation'].merge!('goal_sha256' => records[:intent]['goal_sha256'], 'initial_goal_sent' => true)
    end
    runner.send(:write_portal_manifest, slug, manifest)
    stat = File.stat(File.join(workspace, 'work', slug))
    records[:completion].merge!('trackingDevice' => stat.dev, 'trackingInode' => stat.ino)
    paths = write_pi_creation_records(root, workspace, records)
    FileUtils.mkdir_p(authority, mode: 0700)
    authority_path = File.join(authority, slug + '.json')
    File.write(authority_path, JSON.generate('schema' => 2, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => workspace, 'slug' => slug,
      'session_incarnation' => 'a' * 64, 'binding_revision' => 2, 'generation' => 'e' * 64, 'state' => 'ready'), mode: 'w', perm: 0600)
    [runner, records, paths, authority_path]
  end

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
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', '2026-06-06-demo', 'portal.yml')))
      refute(manifest.key?('codex'))
      refute(manifest.key?('creation'))
      assert_equal([], manifest.fetch('artifacts'))
      assert_equal(1, manifest.fetch('repositories').length)

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

  def test_worktree_add_preserves_one_immutable_base_when_readding_registered_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug, tracking_only: true)
      commit_tracking(workspace, slug, lifecycle: 'active')
      base_sha = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      runner.worktree_add(
        slug,
        'sample',
        as_is: true,
        name: nil,
        branch: slug,
        base: 'master',
        fetch: false
      )
      runner.worktree_remove(slug, 'sample', as_is: true, force: false)
      source = File.join(workspace, 'source-sample')
      assert_git_success('git', '-C', source, 'commit', '--allow-empty', '-m', 'advance default')
      assert_git_success('git', "--git-dir=#{repository}", 'fetch', 'origin',
                         '+refs/heads/master:refs/heads/master', '+refs/heads/master:refs/remotes/origin/master')
      refute_equal(base_sha, git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip)
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: slug, base: nil, fetch: false)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      metadata = manifest.fetch('repositories').fetch(0)
      assert_equal(base_sha, metadata['initial_base_sha'])
      assert_equal(slug, metadata['branch'])
      assert_equal(base_sha, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip)
    end
  end

  def test_worktree_add_recovers_unregistered_checkout_without_inventing_a_base
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      head = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success('git', "--git-dir=#{repository}", 'worktree', 'add', '-b', slug, path, head)

      error = assert_raises(DevSession::Error) do
        runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: slug, base: 'master', fetch: false)
      end
      assert_includes(error.message, 'historical base requires reviewed structured evidence')
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: slug, base: nil, fetch: false)

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      metadata = manifest.fetch('repositories').fetch(0)
      refute(metadata.key?('initial_base_sha'))
      assert_equal(slug, metadata.fetch('branch'))
      assert_equal(head, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip)
      runner.worktree_remove(slug, 'sample', as_is: true, force: false)
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: slug, base: nil, fetch: false)
      restored = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(restored.fetch('repositories').first.key?('initial_base_sha'))
      assert_equal(head, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip)
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
