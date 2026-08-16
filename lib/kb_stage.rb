# frozen_string_literal: true

require 'base64'
require 'fileutils'
require 'json'
require 'net/http'
require 'open3'
require 'securerandom'
require 'time'
require 'uri'

module KbStage
  class Error < StandardError; end

  ROOT = File.expand_path('..', __dir__)
  CONTAINER = 'kb-staging'
  CONTAINERCTL = '/run/current-system/sw/bin/kb-staging-containerctl'
  SITES = %w[cz org].freeze
  DEFAULT_STATE_DIR = File.expand_path('~/.local/state/kb-stage')
  DEFAULT_CODEX_DIR = File.expand_path('~/.codex')
  MANAGED_REF_PATTERN = /\A(?:master|[0-9a-f]{40})\z/

  module_function

  def state_dir
    ENV.fetch('KB_STAGE_STATE_DIR', DEFAULT_STATE_DIR)
  end

  def codex_dir
    ENV.fetch('KB_STAGE_CODEX_DIR', DEFAULT_CODEX_DIR)
  end

  def owner_path
    File.join(state_dir, 'owner.json')
  end

  def mirror_path
    File.join(state_dir, 'mirror.json')
  end

  def lock_path
    File.join(state_dir, 'lock')
  end

  def pending_release_path
    File.join(state_dir, 'pending-release.json')
  end

  def managed_ref_path
    File.join(state_dir, 'credentials', 'managed-repository.ref')
  end

  def current_slug
    slug = ENV['VPSFREE_DEV_SESSION_SLUG']
    raise Error, 'VPSFREE_DEV_SESSION_SLUG is not set' if slug.nil? || slug.empty?

    output, status = Open3.capture2(File.join(ROOT, 'bin/dev-session'), 'current')
    unless status.success? && output.strip == slug
      raise Error, 'the active dev-session does not match VPSFREE_DEV_SESSION_SLUG'
    end

    slug
  end

  def owner
    JSON.parse(File.read(owner_path))
  rescue Errno::ENOENT
    nil
  rescue JSON::ParserError => e
    raise Error, "invalid staging ownership state: #{e.message}"
  end

  def verify_current_owner!
    expected = current_slug
    current = owner
    raise Error, 'KB staging is not owned by a development session' unless current
    return true if current.fetch('slug') == expected

    raise Error, "KB staging is owned by #{current.fetch('slug')}, not #{expected}"
  end

  def with_lock
    FileUtils.mkdir_p(state_dir, mode: 0o700)
    File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
      lock.flock(File::LOCK_EX)
      yield
    end
  end

  def claim!
    with_lock do
      slug = current_slug
      current = owner
      if current && current.fetch('slug') != slug
        raise Error, "KB staging is already owned by #{current.fetch('slug')}"
      end

      unless current
        write_json(owner_path, 'slug' => slug, 'claimed_at' => Time.now.utc.iso8601)
      end
    end
  end

  def release!(discard_pending: false)
    with_owned_lock do
      if File.exist?(pending_release_path) && !discard_pending
        raise Error, 'a staged release is pending; promote it or pass --discard-pending'
      end

      yield if block_given?
      FileUtils.rm_f(owner_path)
      FileUtils.rm_f(pending_release_path) if discard_pending
    end
  end

  def with_owned_lock(invalidate_pending: false)
    with_lock do
      verify_current_owner!
      FileUtils.rm_f(pending_release_path) if invalidate_pending
      yield
    end
  end

  def with_staging_mutation(&block)
    with_owned_lock(invalidate_pending: true, &block)
  end

  def ensure_credentials!
    credentials_dir = File.join(state_dir, 'credentials')
    FileUtils.mkdir_p(credentials_dir, mode: 0o755)
    FileUtils.chmod(0o755, credentials_dir)
    FileUtils.mkdir_p(codex_dir, mode: 0o700)

    SITES.each do |site|
      password_path = File.join(codex_dir, "codex-kb-staging-#{site}-aither-password")
      users_path = File.join(credentials_dir, "#{site}.users.auth.php")
      next if File.exist?(password_path) && File.exist?(users_path)

      password = SecureRandom.urlsafe_base64(36)
      hash, status = Open3.capture2('openssl', 'passwd', '-6', '-stdin', stdin_data: password)
      raise Error, 'openssl failed to hash the staging password' unless status.success?

      atomic_write(password_path, "#{password}\n", 0o600)
      user = "aither:#{hash.strip}:Codex staging API:codex@localhost:admin,user\n"
      atomic_write(users_path, user, 0o644)
    end
  end

  def validate_managed_ref!(ref)
    return ref if ref.is_a?(String) && ref.match?(MANAGED_REF_PATTERN)

    raise Error, 'managed repository ref must be master or a full lowercase commit OID'
  end

  def write_managed_ref!(ref)
    atomic_write(managed_ref_path, "#{validate_managed_ref!(ref)}\n", 0o644)
    ref
  end

  def read_managed_ref
    content = File.read(managed_ref_path, encoding: Encoding::UTF_8)
    validate_managed_ref!(content.sub(/[\r\n]+\z/, ''))
  rescue Errno::ENOENT
    nil
  end

  def prepare_managed_ref!
    pending = JSON.parse(File.read(pending_release_path))
    write_managed_ref!(pending.fetch('managed_ref', 'master'))
  rescue Errno::ENOENT
    write_managed_ref!('master')
  rescue JSON::ParserError => e
    raise Error, "invalid pending release state: #{e.message}"
  end

  def container_running?(runner: Open3.method(:capture2))
    output, status = runner.call('nixos-container', 'status', CONTAINER)
    status.success? && output.strip == 'up'
  end

  def write_json(path, value)
    atomic_write(path, "#{JSON.pretty_generate(value)}\n", 0o600)
  end

  def atomic_write(path, content, mode)
    FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
    temporary = "#{path}.#{Process.pid}.tmp"
    File.open(temporary, File::WRONLY | File::CREAT | File::TRUNC, mode) do |file|
      file.write(content)
      file.flush
      file.fsync
    end
    File.rename(temporary, path)
  ensure
    FileUtils.rm_f(temporary) if defined?(temporary)
  end

  class LanguageLinks
    SITE_URLS = {
      'cz' => 'http://kb-cs.aitherdev.int.vpsfree.cz',
      'org' => 'http://kb-en.aitherdev.int.vpsfree.cz'
    }.freeze
    PAGE_TAG = /<page>\s*([^<]+?)\s*<\/page>/

    def initialize(fetcher: nil, out: $stdout)
      @fetcher = fetcher || method(:fetch)
      @out = out
    end

    def warm_and_verify(czech_pages)
      pairs = czech_pages.filter_map do |id, content|
        target = content.match(PAGE_TAG)&.captures&.first
        [id, target] if target
      end

      warm_and_verify_pairs(pairs)
    end

    def warm_and_verify_pairs(pairs)
      pairs = pairs.to_a

      pairs.each do |czech_id, english_id|
        english_url = page_url('org', english_id)
        czech_url = page_url('cz', czech_id)
        @fetcher.call(english_url)
        @fetcher.call(czech_url)
        [@fetcher.call(english_url), @fetcher.call(czech_url)].each do |html|
          unless html.include?(%{href="#{english_url}"}) && html.include?(%{href="#{czech_url}"})
            raise Error, "staging language links are incomplete for #{czech_id} / #{english_id}"
          end
        end
      end
      @out.puts("warmed and verified #{pairs.length} Czech/English page pairs")
      pairs.length
    end

    private

    def page_url(site, id)
      path = id.split(':').map { |part| URI.encode_uri_component(part) }.join('/')
      "#{SITE_URLS.fetch(site)}/#{path}"
    end

    def fetch(url)
      response = Net::HTTP.get_response(URI(url))
      return response.body if response.is_a?(Net::HTTPSuccess)

      raise Error, "unable to render staging page #{url}: HTTP #{response.code}"
    rescue SystemCallError, Timeout::Error => e
      raise Error, "unable to render staging page #{url}: #{e.message}"
    end
  end

  class Mirror
    def initialize(client_factory:, out: $stdout)
      @client_factory = client_factory
      @out = out
    end

    def reset!
      KbStage.with_staging_mutation do
        raise Error, 'start the staging container before resetting it' unless KbStage.container_running?
        KbStage.write_managed_ref!('master')
        clear_state!
        mirror_all
      end
    end

    private

    def clear_state!
      return if system('sudo', CONTAINERCTL, 'clear')

      raise Error, 'failed to clear the staging DokuWiki state'
    end

    def mirror_all
      pages_by_site = {}
      result = SITES.to_h do |site|
        source = @client_factory.call(site)
        target = @client_factory.call("#{site}-staging")
        pages = fetch_pages(source)
        pages_by_site[site] = pages
        verify_identity!(target)
        replace_pages(target, pages)
        [site, { 'pages' => pages.length }]
      end

      media = fetch_media(@client_factory.call('cz'))
      media_target = @client_factory.call('cz-staging')
      verify_identity!(media_target)
      replace_media(media_target, media)
      result['language_pairs'] = LanguageLinks.new(out: @out).warm_and_verify(
        pages_by_site.fetch('cz')
      )
      result['shared_media'] = media.length
      result['mirrored_at'] = Time.now.utc.iso8601
      KbStage.write_json(KbStage.mirror_path, result)
      result
    end

    def fetch_pages(client)
      client.call('core.listPages', namespace: '', depth: 20).to_h do |entry|
        id = entry.fetch('id')
        [id, client.call('core.getPage', page: id)]
      end
    end

    def fetch_media(client)
      client.call('core.listMedia', namespace: '', depth: 20).to_h do |entry|
        id = entry.fetch('id')
        [id, Base64.strict_decode64(client.call('core.getMedia', media: id))]
      end
    end

    def replace_pages(client, pages)
      existing = client.call('core.listPages', namespace: '', depth: 20)
      existing.each do |entry|
        result = client.call(
          'core.savePage',
          page: entry.fetch('id'), text: '', summary: 'Reset staging mirror', isminor: false
        )
        raise Error, "failed to remove staging page #{entry.fetch('id')}" unless result == true
      end
      pages.each do |id, content|
        result = client.call(
          'core.savePage',
          page: id, text: content, summary: 'Mirror production to staging', isminor: false
        )
        raise Error, "failed to mirror staging page #{id}" unless result == true
      end
      @out.puts("mirrored #{pages.length} pages")
    end

    def replace_media(client, media)
      existing = client.call('core.listMedia', namespace: '', depth: 20)
      existing.each do |entry|
        result = client.call('core.deleteMedia', media: entry.fetch('id'))
        raise Error, "failed to remove staging media #{entry.fetch('id')}" unless result == true
      end
      media.each do |id, content|
        result = client.call(
          'core.saveMedia', media: id, base64: Base64.strict_encode64(content), overwrite: false
        )
        raise Error, "failed to mirror staging media #{id}" unless result == true
      end
      @out.puts("mirrored #{media.length} shared media objects")
    end

    def verify_identity!(client)
      identity = client.call('core.whoAmI')
      login = identity.is_a?(Hash) ? identity['login'] : nil
      raise Error, 'DokuWiki staging API identity is anonymous' if login.nil? || login.empty?
    end
  end
end
