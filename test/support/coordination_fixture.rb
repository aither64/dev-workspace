# frozen_string_literal: true

module CoordinationFixture
  def setup
    @tmp = Dir.mktmpdir
    @state = File.join(@tmp, 'state')
    @workspace = File.join(@tmp, 'workspace')
    @package = File.join(@tmp, 'package')
    @profile = File.join(@tmp, 'profile')
    [@state, @workspace, @package].each { |path| FileUtils.mkdir_p(path, mode: 0o700) }
    File.symlink(@package, @profile)
    @remote = File.join(@tmp, 'remote.git')
    git('init', '--bare', '--initial-branch=master', @remote)
    seed = File.join(@tmp, 'seed')
    git('clone', @remote, seed)
    git('-C', seed, 'config', 'user.name', 'Fixture')
    git('-C', seed, 'config', 'user.email', 'fixture@example.test')
    File.write(File.join(seed, 'base'), 'base')
    git('-C', seed, 'add', 'base')
    git('-C', seed, 'commit', '-m', 'initial fixture')
    git('-C', seed, 'push', 'origin', 'master')
    @bare = File.join(@workspace, 'repos/code.git')
    FileUtils.mkdir_p(File.dirname(@bare))
    git('clone', '--bare', @remote, @bare)
    @base = git('-C', @bare, 'rev-parse', 'master').strip
    @catalog = {'schema' => 1, 'adapters' => {}, 'machines' => {}, 'repositories' => {'code' => {'remote' => @remote}}, 'profileMachine' => nil}
    @entry = {'name' => 'fixture', 'socket' => '/fixture/codex.sock', 'authority' => File.join(@state, 'authority'), 'portalUrl' => 'https://workspace.example.test'}
    install_catalog
    @engine = engine
  end

  def teardown
    FileUtils.remove_entry(@tmp)
  end

  def git(*args)
    out, err, status = Open3.capture3('git', *args)
    raise err unless status.success?
    out
  end

  def install_catalog
    path = File.join(@package, 'share/dev-workspace/coordination.json')
    FileUtils.mkdir_p(File.dirname(path))
    File.write(path, JSON.generate(@catalog))
  end

  def engine
    WorkspaceCoordination::Engine.new(state: @state, package: @package, profile: @profile,
      workspace: @workspace, entry: @entry, fixtures: true)
  end

  def owner(slug)
    directory = File.join(@workspace, 'work', slug)
    FileUtils.mkdir_p(directory)
    manifest = {'slug' => slug, 'repositories' => [{'name' => 'code', 'project' => 'code', 'branch' => slug, 'default_branch' => 'master'}],
      'codex' => {'thread_id' => SecureRandom.uuid, 'socket_path' => @entry['socket']}}
    File.write(File.join(directory, 'portal.yml'), YAML.dump(manifest))
    File.write(File.join(directory, 'review.md'), 'reviewed fixture')
    File.write(File.join(directory, 'state.md'), "---\nlifecycle: active\n---\n")
    @engine.owner(slug)
  end

  def evidence(owner)
    path = File.join(@workspace, 'work', owner['slug'], 'review.md')
    [{'kind' => 'review', 'path' => path, 'sha256' => Digest::SHA256.file(path).hexdigest}]
  end

  def request(values)
    {'schema' => 1, 'requestId' => SecureRandom.uuid}.merge(values)
  end

  def with_time(value)
    original = Time.method(:now)
    Time.define_singleton_method(:now) { value }
    yield
  ensure
    Time.define_singleton_method(:now, original)
  end

  def approval(owner, snapshot, kind: 'merge', scope: nil)
    @engine.mutate(owner, 'approve', request('kind' => kind, 'scope' => scope || [{'repository' => 'code', 'target' => 'refs/heads/master'}],
      'source' => {'kind' => 'user_instruction', 'reference' => 'fixture approval', 'quote' => 'integrate code into master'}, 'reviewedSnapshot' => snapshot['id']))['id']
  end

  def feature(owner)
    checkout = File.join(@tmp, owner['slug'])
    git('-C', @bare, 'worktree', 'add', '-b', owner['slug'], checkout, @base)
    git('-C', checkout, 'config', 'user.name', 'Fixture')
    git('-C', checkout, 'config', 'user.email', 'fixture@example.test')
    File.write(File.join(checkout, owner['slug']), owner['slug'])
    git('-C', checkout, 'add', owner['slug'])
    git('-C', checkout, 'commit', '-m', 'fixture feature')
    checkout
  end

  def submit_merge(owner)
    feature(owner)
    snap = @engine.prepare_merge(owner, 'code', evidence(owner))
    @engine.mutate(owner, 'submit', request('kind' => 'merge', 'dependsOn' => [], 'approvalId' => approval(owner, snap), 'reservationId' => nil, 'snapshot' => snap))
  end

end
