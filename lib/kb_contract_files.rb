# frozen_string_literal: true

module KbContractFiles
  PAGE_SEGMENT = /\A[a-z0-9_][a-z0-9_.-]*\z/
  SEMANTIC_ID = /\A[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*\z/
  CAPTURE_ID = /\A[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*(?:\/[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*)+\z/
  MEDIA_EXTENSION = /\.(?:gif|jpe?g|png|svg|webp)\z/i

  module_function

  def validate_page_id!(page_id)
    segments = page_id.is_a?(String) ? page_id.split(':', -1) : []
    unless !segments.empty? && segments.all? do |segment|
      segment.match?(PAGE_SEGMENT) && segment != '.' && segment != '..'
    end
      abort "invalid DokuWiki page ID #{page_id.inspect}"
    end
    page_id
  end

  def validate_semantic_id!(semantic_id)
    abort "invalid semantic path ID #{semantic_id.inspect}" unless semantic_id.is_a?(String) && semantic_id.match?(SEMANTIC_ID)
    semantic_id
  end

  def validate_capture_id!(capture_id)
    abort "invalid capture asset ID #{capture_id.inspect}" unless capture_id.is_a?(String) && capture_id.match?(CAPTURE_ID)

    capture_id
  end

  def validate_media_id!(media_id)
    validate_page_id!(media_id)
    abort "DokuWiki media ID has no supported extension #{media_id.inspect}" unless media_id.match?(MEDIA_EXTENSION)

    media_id
  end

  def path_within(root, relative)
    abort "invalid relative file path #{relative.inspect}" unless relative.is_a?(String) && !relative.empty?
    abort "absolute file path is not allowed: #{relative}" if relative.start_with?('/')

    expanded_root = File.expand_path(root)
    expanded = File.expand_path(relative, expanded_root)
    unless expanded.start_with?("#{expanded_root}/")
      abort "file path escapes its root: #{relative}"
    end
    expanded
  end
end
