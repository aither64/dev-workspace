# frozen_string_literal: true

require 'json'
require 'minitest/autorun'
require 'stringio'
require 'tmpdir'

load File.expand_path('../bin/kb-page', __dir__)

class KbPageTest < Minitest::Test
  class FakeClient
    attr_reader :calls

    def initialize
      @expected = []
      @calls = []
    end

    def expect(method, params = nil, result:)
      @expected << [method, params, result]
    end

    def call(method, params = nil)
      @calls << [method, params]
      expected = @expected.shift

      raise "unexpected call #{[method, params].inspect}" unless expected

      exp_method, exp_params, result = expected
      unless exp_method == method && exp_params == params
        raise "expected #{[exp_method, exp_params].inspect}, got #{[method, params].inspect}"
      end

      raise result if result.is_a?(Exception)

      result
    end

    def assert_done(test)
      test.assert_empty(@expected)
    end
  end

  def test_wiki_config_selects_known_kbs
    assert_equal('https://kb.vpsfree.cz', KbPage.wiki_config('cz').fetch(:url))
    assert_equal('https://kb.vpsfree.org', KbPage.wiki_config('org').fetch(:url))

    assert_raises(KbPage::Error) { KbPage.wiki_config('missing') }
  end

  def test_json_rpc_client_uses_bearer_token_without_putting_token_in_body
    Dir.mktmpdir do |dir|
      token_path = File.join(dir, 'token')
      File.write(token_path, "secret-token\n")
      captured = {}
      transport = lambda do |uri, headers, body|
        captured[:uri] = uri
        captured[:headers] = headers
        captured[:body] = body
        [200, JSON.generate('result' => 'ok')]
      end
      client = KbPage::JsonRpcClient.new(
        base_url: 'https://kb.example',
        token_path:,
        transport:
      )

      assert_equal('ok', client.call('core.whoAmI'))
      assert_equal('https://kb.example/lib/exe/jsonrpc.php', captured.fetch(:uri).to_s)
      assert_equal('Bearer secret-token', captured.fetch(:headers).fetch('Authorization'))
      refute_includes(captured.fetch(:body), 'secret-token')
    end
  end

  def test_json_rpc_client_uses_basic_auth_for_staging
    Dir.mktmpdir do |dir|
      password_path = File.join(dir, 'password')
      File.write(password_path, "secret-password\n")
      captured = {}
      transport = lambda do |_uri, headers, body|
        captured[:headers] = headers
        captured[:body] = body
        [200, JSON.generate('result' => 'ok')]
      end
      client = KbPage::JsonRpcClient.new(
        base_url: 'http://kb.example',
        username: 'aither',
        password_path:,
        transport:
      )

      assert_equal('ok', client.call('core.whoAmI'))
      expected = "Basic #{Base64.strict_encode64('aither:secret-password')}"
      assert_equal(expected, captured.fetch(:headers).fetch('Authorization'))
      refute_includes(captured.fetch(:body), 'secret-password')
    end
  end

  def test_json_rpc_client_parses_json_rpc_error_from_http_error_response
    Dir.mktmpdir do |dir|
      token_path = File.join(dir, 'token')
      File.write(token_path, "secret-token\n")
      transport = lambda do |_uri, _headers, _body|
        [
          400,
          JSON.generate(
            'error' => {
              'code' => 121,
              'message' => 'Page does not exist'
            }
          )
        ]
      end
      client = KbPage::JsonRpcClient.new(
        base_url: 'https://kb.example',
        token_path:,
        transport:
      )

      error = assert_raises(KbPage::RpcError) do
        client.call('core.getPageInfo', page: 'drafts:missing')
      end

      assert_equal(121, error.code)
      assert_equal('Page does not exist', error.rpc_message)
    end
  end

  def test_get_refuses_missing_page
    client = FakeClient.new
    client.expect(
      'core.getPageInfo',
      { page: 'drafts:missing' },
      result: not_found
    )

    error = assert_raises(KbPage::Error) do
      run_with(client, 'get', '--wiki', 'cz', 'drafts:missing')
    end

    assert_match(/page does not exist/, error.message)
    client.assert_done(self)
  end

  def test_get_reads_existing_page
    client = FakeClient.new
    client.expect('core.getPageInfo', { page: 'drafts:test' }, result: {})
    client.expect('core.getPage', { page: 'drafts:test' }, result: "hello\n")

    out = run_with(client, 'get', '--wiki', 'cz', 'drafts:test')

    assert_equal("hello\n", out)
    client.assert_done(self)
  end

  def test_save_creates_page_on_owned_staging_without_production_approval
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect(
        'core.getPageInfo',
        { page: 'drafts:test' },
        result: not_found
      )
      client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
      client.expect('core.aclCheck', { page: 'drafts:test' }, result: 4)
      client.expect(
        'core.savePage',
        {
          page: 'drafts:test',
          text: 'hello',
          summary: 'create draft',
          isminor: false
        },
        result: true
      )

      out = run_with(
        client,
        'save',
        '--wiki',
        'cz-staging',
        'drafts:test',
        file,
        '--summary',
        'create draft',
        '--create'
      )

      assert_equal("saved drafts:test on cz-staging\n", out)
      client.assert_done(self)
    end
  end

  def test_save_requires_true_save_page_result
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect(
        'core.getPageInfo',
        { page: 'drafts:test' },
        result: not_found
      )
      client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
      client.expect('core.aclCheck', { page: 'drafts:test' }, result: 4)
      client.expect(
        'core.savePage',
        {
          page: 'drafts:test',
          text: 'hello',
          summary: 'create draft',
          isminor: false
        },
        result: false
      )

      error = assert_raises(KbPage::Error) do
        run_with(
          client,
          'save',
          '--wiki',
          'cz-staging',
          'drafts:test',
          file,
          '--summary',
          'create draft',
          '--create'
        )
      end

      assert_match(/did not report success/, error.message)
      client.assert_done(self)
    end
  end

  def test_save_refuses_non_draft_without_approval
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect(
        'core.getPageInfo',
        { page: 'public:test' },
        result: not_found
      )

      error = assert_raises(KbPage::Error) do
        run_with(
          client,
          'save',
          '--wiki',
          'cz',
          'public:test',
          file,
          '--summary',
          'create public',
          '--create'
        )
      end

      assert_match(/approved-production/, error.message)
      assert_equal([['core.getPageInfo', { page: 'public:test' }]], client.calls)
      client.assert_done(self)
    end
  end

  def test_staging_write_requires_current_owner
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect('core.getPageInfo', { page: 'public:test' }, result: not_found)
      runner = KbPage::Runner.new(
        out: StringIO.new,
        err: StringIO.new,
        client_factory: ->(_wiki) { client },
        staging_owner_verifier: -> { raise KbPage::Error, 'owned elsewhere' },
        staging_write_wrapper: ->(&block) { block.call }
      )

      error = assert_raises(KbPage::Error) do
        runner.run(
          ['save', '--wiki', 'cz-staging', 'public:test', file, '--summary', 'test', '--create']
        )
      end
      assert_match(/owned elsewhere/, error.message)
    end
  end

  def test_staging_write_with_equals_form_runs_inside_wrapper
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect('core.getPageInfo', { page: 'public:test' }, result: not_found)
      client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
      client.expect('core.aclCheck', { page: 'public:test' }, result: 4)
      client.expect(
        'core.savePage',
        {
          page: 'public:test', text: 'hello', summary: 'test', isminor: false
        },
        result: true
      )
      wrapped = false
      runner = KbPage::Runner.new(
        out: StringIO.new,
        err: StringIO.new,
        client_factory: ->(_wiki) { client },
        staging_owner_verifier: -> { true },
        staging_write_wrapper: lambda do |&block|
          wrapped = true
          block.call
        end
      )

      runner.run(
        ['save', '--wiki=cz-staging', 'public:test', file, '--summary', 'test', '--create']
      )
      assert(wrapped)
      client.assert_done(self)
    end
  end

  def test_abbreviated_wiki_option_is_rejected
    error = assert_raises(OptionParser::InvalidOption) do
      run_with(FakeClient.new, 'save', '--wik=cz-staging', 'page', 'file', '--create')
    end

    assert_match(/--wik/, error.message)
  end

  def test_save_refuses_create_when_page_exists
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect('core.getPageInfo', { page: 'drafts:test' }, result: {})

      error = assert_raises(KbPage::Error) do
        run_with(
          client,
          'save',
          '--wiki',
          'cz',
          'drafts:test',
          file,
          '--summary',
          'create draft',
          '--create'
        )
      end

      assert_match(/already exists/, error.message)
      client.assert_done(self)
    end
  end

  def test_save_refuses_update_when_page_is_missing
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect(
        'core.getPageInfo',
        { page: 'drafts:test' },
        result: not_found
      )

      error = assert_raises(KbPage::Error) do
        run_with(
          client,
          'save',
          '--wiki',
          'cz',
          'drafts:test',
          file,
          '--summary',
          'update draft',
          '--update'
        )
      end

      assert_match(/does not exist/, error.message)
      client.assert_done(self)
    end
  end

  def test_delete_uses_empty_save_page
    client = FakeClient.new
    client.expect('core.getPageInfo', { page: 'drafts:old' }, result: {})
    client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
    client.expect('core.aclCheck', { page: 'drafts:old' }, result: 16)
    client.expect(
      'core.savePage',
      {
        page: 'drafts:old',
        text: '',
        summary: 'remove draft',
        isminor: false
      },
      result: true
    )

    out = run_with(
      client,
      'delete',
      '--wiki',
      'cz-staging',
      'drafts:old',
      '--summary',
      'remove draft',
      '--yes'
    )

    assert_equal("deleted drafts:old on cz-staging\n", out)
    client.assert_done(self)
  end

  def test_delete_requires_yes
    error = assert_raises(KbPage::Error) do
      run_with(
        FakeClient.new,
        'delete',
        '--wiki',
        'cz',
        'drafts:old',
        '--summary',
        'remove draft'
      )
    end

    assert_match(/delete requires --yes/, error.message)
  end

  def test_delete_refuses_non_draft_without_approval
    client = FakeClient.new
    client.expect('core.getPageInfo', { page: 'public:old' }, result: {})

    error = assert_raises(KbPage::Error) do
      run_with(
        client,
        'delete',
        '--wiki',
        'cz',
        'public:old',
        '--summary',
        'remove page',
        '--yes'
      )
    end

    assert_match(/approved-production/, error.message)
    client.assert_done(self)
  end

  def test_rename_copies_content_then_deletes_source
    client = FakeClient.new
    client.expect('core.getPageInfo', { page: 'drafts:old' }, result: {})
    client.expect('core.getPageInfo', { page: 'drafts:new' }, result: not_found)
    client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
    client.expect('core.aclCheck', { page: 'drafts:old' }, result: 16)
    client.expect('core.aclCheck', { page: 'drafts:new' }, result: 4)
    client.expect('core.getPage', { page: 'drafts:old' }, result: "body\n")
    client.expect(
      'core.savePage',
      {
        page: 'drafts:new',
        text: "body\n",
        summary: 'move draft',
        isminor: false
      },
      result: true
    )
    client.expect(
      'core.savePage',
      {
        page: 'drafts:old',
        text: '',
        summary: 'move draft',
        isminor: false
      },
      result: true
    )

    out = run_with(
      client,
      'rename',
      '--wiki',
      'cz-staging',
      'drafts:old',
      'drafts:new',
      '--summary',
      'move draft'
    )

    assert_equal("renamed drafts:old to drafts:new on cz-staging\n", out)
    client.assert_done(self)
  end

  def test_rename_refuses_non_draft_without_approval
    client = FakeClient.new
    client.expect('core.getPageInfo', { page: 'drafts:old' }, result: {})
    client.expect('core.getPageInfo', { page: 'public:new' }, result: not_found)

    error = assert_raises(KbPage::Error) do
      run_with(
        client,
        'rename',
        '--wiki',
        'cz',
        'drafts:old',
        'public:new',
        '--summary',
        'publish page'
      )
    end

    assert_match(/approved-production/, error.message)
    client.assert_done(self)
  end

  def test_rename_does_not_delete_source_when_destination_save_fails
    client = FakeClient.new
    client.expect('core.getPageInfo', { page: 'drafts:old' }, result: {})
    client.expect('core.getPageInfo', { page: 'drafts:new' }, result: not_found)
    client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
    client.expect('core.aclCheck', { page: 'drafts:old' }, result: 16)
    client.expect('core.aclCheck', { page: 'drafts:new' }, result: 4)
    client.expect('core.getPage', { page: 'drafts:old' }, result: "body\n")
    client.expect(
      'core.savePage',
      {
        page: 'drafts:new',
        text: "body\n",
        summary: 'move draft',
        isminor: false
      },
      result: false
    )

    error = assert_raises(KbPage::Error) do
      run_with(
        client,
        'rename',
        '--wiki',
        'cz-staging',
        'drafts:old',
        'drafts:new',
        '--summary',
        'move draft'
      )
    end

    assert_match(/did not report success/, error.message)
    client.assert_done(self)
  end

  def test_rename_refuses_existing_destination_without_overwrite
    client = FakeClient.new
    client.expect('core.getPageInfo', { page: 'drafts:old' }, result: {})
    client.expect('core.getPageInfo', { page: 'drafts:new' }, result: {})

    error = assert_raises(KbPage::Error) do
      run_with(
        client,
        'rename',
        '--wiki',
        'cz',
        'drafts:old',
        'drafts:new',
        '--summary',
        'move draft'
      )
    end

    assert_match(/destination page already exists/, error.message)
    client.assert_done(self)
  end

  def test_non_draft_write_with_approval_continues
    with_temp_file('hello') do |file|
      client = FakeClient.new
      client.expect(
        'core.getPageInfo',
        { page: 'public:test' },
        result: not_found
      )
      client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
      client.expect('core.aclCheck', { page: 'public:test' }, result: 4)
      client.expect(
        'core.savePage',
        {
          page: 'public:test',
          text: 'hello',
          summary: 'create public',
          isminor: false
        },
        result: true
      )

      run_with(
        client,
        'save',
        '--wiki',
        'cz',
        'public:test',
        file,
        '--summary',
        'create public',
        '--create',
        '--approved-production'
      )

      client.assert_done(self)
    end
  end

  def test_production_write_rejects_multiline_summary
    with_temp_file('hello') do |file|
      client = FakeClient.new

      error = assert_raises(KbPage::Error) do
        run_with(
          client,
          'save',
          '--wiki',
          'org',
          'public:test',
          file,
          '--summary',
          "first line\nsecond line",
          '--create',
          '--approved-production'
        )
      end

      assert_match(/single line/, error.message)
      client.assert_done(self)
    end
  end

  def test_media_info_requests_hash
    client = FakeClient.new
    client.expect(
      'core.getMediaInfo',
      { media: 'drafts:test.png', hash: true },
      result: { 'id' => 'drafts:test.png', 'hash' => 'abc' }
    )

    out = run_with(client, 'media-info', '--wiki', 'cz', 'drafts:test.png')

    assert_equal("{\n  \"id\": \"drafts:test.png\",\n  \"hash\": \"abc\"\n}\n", out)
    client.assert_done(self)
  end

  def test_media_get_decodes_file_and_refuses_local_overwrite
    Dir.mktmpdir do |dir|
      file = File.join(dir, 'image.png')
      data = png_data
      client = FakeClient.new
      client.expect(
        'core.getMedia',
        { media: 'drafts:test.png' },
        result: Base64.strict_encode64(data)
      )

      out = run_with(
        client,
        'media-get',
        '--wiki',
        'cz-staging',
        'drafts:test.png',
        file
      )

      assert_equal(data, File.binread(file))
      assert_match(/saved drafts:test\.png/, out)
      client.assert_done(self)

      error = assert_raises(KbPage::Error) do
        run_with(
          FakeClient.new,
          'media-get',
          '--wiki',
          'cz',
          'drafts:test.png',
          file
        )
      end
      assert_match(/local file already exists/, error.message)
    end
  end

  def test_media_save_creates_draft_without_overwrite
    with_temp_media(png_data) do |file|
      client = FakeClient.new
      client.expect(
        'core.getMediaInfo',
        { media: 'drafts:test.png' },
        result: media_not_found
      )
      client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
      client.expect('core.aclCheck', { page: 'drafts:test.png' }, result: 8)
      client.expect(
        'core.saveMedia',
        {
          media: 'drafts:test.png',
          base64: Base64.strict_encode64(png_data),
          overwrite: false
        },
        result: true
      )

      out = run_with(
        client,
        'media-save',
        '--wiki',
        'cz-staging',
        'drafts:test.png',
        file,
        '--create'
      )

      assert_equal("saved media drafts:test.png on cz-staging\n", out)
      client.assert_done(self)
    end
  end

  def test_media_save_refuses_existing_media
    with_temp_media(png_data) do |file|
      client = FakeClient.new
      client.expect('core.getMediaInfo', { media: 'drafts:test.png' }, result: {})

      error = assert_raises(KbPage::Error) do
        run_with(
          client,
          'media-save',
          '--wiki',
          'cz',
          'drafts:test.png',
          file,
          '--create'
        )
      end

      assert_match(/already exists/, error.message)
      client.assert_done(self)
    end
  end

  def test_media_save_updates_existing_staging_media
    with_temp_media(png_data) do |file|
      client = FakeClient.new
      client.expect('core.getMediaInfo', { media: 'screenshots:test.png' }, result: {})
      client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
      client.expect('core.aclCheck', { page: 'screenshots:test.png' }, result: 16)
      client.expect(
        'core.saveMedia',
        {
          media: 'screenshots:test.png',
          base64: Base64.strict_encode64(png_data),
          overwrite: true
        },
        result: true
      )

      out = run_with(
        client,
        'media-save',
        '--wiki',
        'cz-staging',
        'screenshots:test.png',
        file,
        '--update'
      )

      assert_equal("saved media screenshots:test.png on cz-staging\n", out)
      client.assert_done(self)
    end
  end

  def test_media_save_refuses_non_draft_without_approval
    with_temp_media(png_data) do |file|
      client = FakeClient.new
      client.expect(
        'core.getMediaInfo',
        { media: 'screenshots:test.png' },
        result: media_not_found
      )

      error = assert_raises(KbPage::Error) do
        run_with(
          client,
          'media-save',
          '--wiki',
          'cz',
          'screenshots:test.png',
          file,
          '--create'
        )
      end

      assert_match(/approved-production/, error.message)
      client.assert_done(self)
    end
  end

  def test_media_save_validates_extension_and_content
    with_temp_media('not a PNG') do |file|
      error = assert_raises(KbPage::Error) do
        run_with(
          FakeClient.new,
          'media-save',
          '--wiki',
          'cz',
          'drafts:test.png',
          file,
          '--create'
        )
      end

      assert_match(/does not match/, error.message)
    end
  end

  def test_media_delete_deletes_draft
    client = FakeClient.new
    client.expect('core.getMediaInfo', { media: 'drafts:test.png' }, result: {})
    client.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
    client.expect('core.aclCheck', { page: 'drafts:test.png' }, result: 16)
    client.expect('core.deleteMedia', { media: 'drafts:test.png' }, result: true)

    out = run_with(
      client,
      'media-delete',
      '--wiki',
      'cz-staging',
      'drafts:test.png',
      '--yes'
    )

    assert_equal("deleted media drafts:test.png on cz-staging\n", out)
    client.assert_done(self)
  end

  private

  def not_found
    KbPage::RpcError.new(1, 'Page does not exist')
  end

  def media_not_found
    KbPage::RpcError.new(221, 'The requested media file does not exist')
  end

  def run_with(client, *argv)
    out = StringIO.new
    runner = KbPage::Runner.new(
      out:,
      err: StringIO.new,
      client_factory: ->(_wiki) { client },
      staging_owner_verifier: -> { true },
      staging_write_wrapper: ->(&block) { block.call }
    )
    runner.run(argv)
    out.string
  end

  def with_temp_file(content)
    Dir.mktmpdir do |dir|
      path = File.join(dir, 'page.txt')
      File.write(path, content)
      yield path
    end
  end

  def with_temp_media(content)
    Dir.mktmpdir do |dir|
      path = File.join(dir, 'image.png')
      File.binwrite(path, content)
      yield path
    end
  end

  def png_data
    "\x89PNG\r\n\x1A\nfixture".b
  end
end
