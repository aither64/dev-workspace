# frozen_string_literal: true

require 'digest'
require 'open3'
require 'thread'

module WorkspaceCoordination
  # Drain both streams even after reaching a bound. Logs never decide whether
  # a mutation happened, and an output limit never kills an activation.
  class Command
    class Log
      FIRST = 1024 * 1024
      TAIL = 3 * 1024 * 1024 - 128

      def initialize
        @first, @tail, @bytes = ''.b, ''.b, 0
        @mutex = Mutex.new
      end

      def append(stream, bytes)
        @mutex.synchronize do
          value = "[#{stream}] ".b + bytes.b
          @bytes += value.bytesize
          keep = [FIRST - @first.bytesize, value.bytesize].min
          @first << value.byteslice(0, keep) if keep > 0
          rest = value.byteslice(keep, value.bytesize - keep)
          @tail << rest if rest
          @tail = @tail.byteslice(-TAIL, TAIL) if @tail.bytesize > TAIL
        end
      end

      def bytes
        marker = @bytes > FIRST + TAIL ? "\n[diagnostics truncated; #{@bytes} bytes drained]\n" : ''
        @first + marker.b + @tail
      end
    end

    def self.run(argv, env: {}, cwd: nil, input: '', limit: 262_144, log: nil, digest: false)
      diagnostics = log || Log.new
      output, error, overflow = ''.b, ''.b, false
      hash = Digest::SHA256.new
      options = cwd ? {chdir: cwd} : {}
      status = nil
      Open3.popen3(env, *argv, **options) do |stdin, stdout, stderr, waiter|
        writer = Thread.new do
          begin
            stdin.write(input.to_s)
          rescue Errno::EPIPE
            # The child's status supplies the failure evidence.
          ensure
            stdin.close
          end
        end
        readers = [Thread.new do
          while (chunk = stdout.read(16_384))
            if digest
              hash.update(chunk)
            elsif output.bytesize + chunk.bytesize <= limit
              output << chunk
            else
              overflow = true
            end
          end
        end, Thread.new do
          while (chunk = stderr.read(16_384))
            diagnostics.append('stderr', chunk)
            error << chunk.byteslice(0, [4096 - error.bytesize, chunk.bytesize].min) if error.bytesize < 4096
          end
        end]
        writer.value
        readers.each(&:value)
        status = waiter.value
      end
      [digest ? "sha256:#{hash.hexdigest}" : output.force_encoding(Encoding::UTF_8), error.force_encoding(Encoding::UTF_8).scrub, status, overflow]
    end
  end
end
