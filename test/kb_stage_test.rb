# frozen_string_literal: true

require 'digest'
require 'minitest/autorun'
require 'stringio'
require 'tmpdir'
require 'yaml'

require_relative '../lib/kb_stage'
load File.expand_path('../bin/kb-page', __dir__)
require_relative '../lib/kb_release'

class KbStageTest < Minitest::Test
  class FakeClient
    def initialize
      @expected = []
    end

    def expect(method, params = nil, result:)
      @expected << [method, params, result]
    end

    def call(method, params = nil)
      expected = @expected.shift
      raise "unexpected call #{[method, params].inspect}" unless expected

      exp_method, exp_params, result = expected
      unless [exp_method, exp_params] == [method, params]
        raise "expected #{[exp_method, exp_params].inspect}, got #{[method, params].inspect}"
      end

      result
    end

    def done?
      @expected.empty?
    end
  end

  def test_claim_serializes_owners_and_release_refuses_pending_bundle
    with_state do
      stub_kb_stage(:current_slug, 'session-one') do
        KbStage.claim!
        assert_equal('session-one', KbStage.owner.fetch('slug'))
        KbStage.claim!
        KbStage.write_json(KbStage.pending_release_path, 'sha256' => 'test')
        assert_raises(KbStage::Error) { KbStage.release! }
        KbStage.release!(discard_pending: true)
        assert_nil(KbStage.owner)
      end
    end
  end

  def test_claim_refuses_a_different_session
    with_state do
      stub_kb_stage(:current_slug, 'session-one') { KbStage.claim! }
      error = stub_kb_stage(:current_slug, 'session-two') do
        assert_raises(KbStage::Error) { KbStage.claim! }
      end
      assert_match(/session-one/, error.message)
    end
  end

  def test_credentials_are_generated_outside_the_repository
    with_state do |state, codex|
      KbStage.ensure_credentials!
      %w[cz org].each do |site|
        password = File.join(codex, "codex-kb-staging-#{site}-aither-password")
        users = File.join(state, 'credentials', "#{site}.users.auth.php")
        assert(File.size?(password))
        assert_match(/\Aaither:\$6\$/, File.read(users))
        assert_equal(0o600, File.stat(password).mode & 0o777)
        assert_equal(0o644, File.stat(users).mode & 0o777)
      end
    end
  end

  def test_release_stages_only_when_production_and_staging_match_source
    with_state do
      Dir.mktmpdir do |release_dir|
        source = "old\n"
        candidate = "new\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 1,
            'wiki' => 'cz',
            'pages' => [
              {
                'id' => 'page',
                'source_revision' => 123,
                'source_sha256' => Digest::SHA256.hexdigest(source),
                'file' => 'page.txt',
                'sha256' => Digest::SHA256.hexdigest(candidate)
              }
            ],
            'media' => []
          )
        )

        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'page' }, result: { 'revision' => 123 })
        production.expect('core.getPage', { page: 'page' }, result: source)
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: source)
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect('core.aclCheck', { page: 'page' }, result: 255)
        staging.expect(
          'core.savePage',
          {
            page: 'page', text: candidate, summary: 'Stage reviewed KB release', isminor: false
          },
          result: true
        )
        staging.expect('core.getPage', { page: 'page' }, result: candidate)

        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        stub_kb_stage(:verify_current_owner!, true) do
          stub_kb_stage(:current_slug, 'session-one') { runner.stage! }
        end

        assert(production.done?)
        assert(staging.done?)
        assert_equal('session-one', JSON.parse(File.read(KbStage.pending_release_path)).fetch('slug'))
      end
    end
  end

  def test_release_library_refuses_promotion_without_explicit_approval
    runner = KbRelease::Runner.new(
      manifest: Object.new,
      client_factory: ->(_name) { raise 'must not connect' },
      out: StringIO.new
    )

    error = assert_raises(KbRelease::Error) { runner.promote! }
    assert_match(/explicit approval/, error.message)
  end

  private

  def stub_kb_stage(name, value)
    original = KbStage.method(name)
    KbStage.define_singleton_method(name) { value }
    yield
  ensure
    KbStage.define_singleton_method(name, original)
  end

  def with_state
    Dir.mktmpdir do |dir|
      state = File.join(dir, 'state')
      codex = File.join(dir, 'codex')
      old_state = ENV['KB_STAGE_STATE_DIR']
      old_codex = ENV['KB_STAGE_CODEX_DIR']
      ENV['KB_STAGE_STATE_DIR'] = state
      ENV['KB_STAGE_CODEX_DIR'] = codex
      yield state, codex
    ensure
      ENV['KB_STAGE_STATE_DIR'] = old_state
      ENV['KB_STAGE_CODEX_DIR'] = old_codex
    end
  end
end
