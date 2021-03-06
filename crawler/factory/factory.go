package factory

import (
	"context"
	"log"

	"github.com/ipfs-search/ipfs-search/crawler"
	"github.com/ipfs-search/ipfs-search/index"
	"github.com/ipfs-search/ipfs-search/index/elasticsearch"
	"github.com/ipfs-search/ipfs-search/instr"
	"github.com/ipfs-search/ipfs-search/queue"
	"github.com/ipfs-search/ipfs-search/queue/amqp"
	"github.com/ipfs-search/ipfs-search/worker"

	"github.com/ipfs/go-ipfs-api"
	"github.com/olivere/elastic/v7"
	samqp "github.com/streadway/amqp"

	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/label"
)

// Factory creates hash and file crawl workers
type Factory struct {
	crawlerConfig *crawler.Config
	pubConnection *amqp.Connection
	conConnection *amqp.Connection
	errChan       chan<- error

	fileIndex      index.Index
	directoryIndex index.Index
	invalidIndex   index.Index

	shell *shell.Shell
	*instr.Instrumentation
}

// New creates a new crawl worker factory
func New(ctx context.Context, config *Config, i *instr.Instrumentation, errc chan<- error) (*Factory, error) {
	ctx, span := i.Tracer.Start(ctx, "crawler.factory.New")
	defer span.End()

	pubConnection, err := amqp.NewConnection(ctx, config.AMQPURL, i)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}

	conConnection, err := amqp.NewConnection(ctx, config.AMQPURL, i)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}
	span.AddEvent(ctx, "amqp-connected")
	log.Printf("Connected to AMQP")

	// Create and configure Ipfs shell
	sh := shell.NewShell(config.IpfsAPI)
	sh.SetTimeout(config.IpfsTimeout)

	es, err := elastic.NewClient(elastic.SetSniff(false), elastic.SetURL(config.ElasticSearchURL))
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}
	log.Printf("Connected to ElasticSearch")
	span.AddEvent(ctx, "elasticsearch-connected")

	indexes := elasticsearch.NewMulti(es, config.Indexes["files"], config.Indexes["directories"], config.Indexes["invalids"])
	span.AddEvent(ctx, "indexes-initialized")

	return &Factory{
		crawlerConfig:   config.CrawlerConfig,
		pubConnection:   pubConnection,
		conConnection:   conConnection,
		errChan:         errc,
		shell:           sh,
		fileIndex:       indexes[0],
		directoryIndex:  indexes[1],
		invalidIndex:    indexes[2],
		Instrumentation: i,
	}, nil
}

func (f *Factory) newCrawler(ctx context.Context) (*crawler.Crawler, error) {
	ctx, span := f.Tracer.Start(ctx, "crawler.factory.newCrawler")
	defer span.End()

	fileQueue, err := f.pubConnection.NewChannelQueue(ctx, "files")
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}

	hashQueue, err := f.pubConnection.NewChannelQueue(ctx, "hashes")
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}

	span.AddEvent(ctx, "publish-queues-created")

	return &crawler.Crawler{
		Config: f.crawlerConfig,
		Shell:  f.shell,

		FileIndex:      f.fileIndex,
		DirectoryIndex: f.directoryIndex,
		InvalidIndex:   f.invalidIndex,

		FileQueue: fileQueue,
		HashQueue: hashQueue,

		Instrumentation: f.Instrumentation,
	}, nil
}

// newWorker generalizes creating new workers; it takes a queue name and a
// crawlFunc, which takes an Indexable and returns the function performing
// the actual crawling
func (f *Factory) newWorker(ctx context.Context, queueName string, crawl CrawlFunc) (worker.Worker, error) {
	ctx, span := f.Tracer.Start(ctx, "crawler.factory.newWorker", trace.WithAttributes(label.String("queue", queueName)))
	defer span.End()

	conQueue, err := f.conConnection.NewChannelQueue(ctx, queueName)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}
	span.AddEvent(ctx, "consume-queue-created")

	c, err := f.newCrawler(ctx)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}

	span.AddEvent(ctx, "crawler-initialized")

	// A MessageWorkerFactory generates a worker for every message in a queue
	messageWorkerFactory := func(msg *samqp.Delivery) worker.Worker {
		return &Worker{
			Crawler:   c,
			Delivery:  msg,
			CrawlFunc: crawl,
		}
	}

	return queue.NewWorker(f.errChan, conQueue, messageWorkerFactory, f.Instrumentation), nil
}

// NewHashWorker returns a new hash crawl worker
func (f *Factory) NewHashWorker(ctx context.Context) (worker.Worker, error) {
	return f.newWorker(ctx, "hashes", func(i *crawler.Indexable) func(context.Context) error {
		return i.CrawlHash
	})
}

// NewFileWorker returns a new file crawl worker
func (f *Factory) NewFileWorker(ctx context.Context) (worker.Worker, error) {
	return f.newWorker(ctx, "files", func(i *crawler.Indexable) func(context.Context) error {
		return i.CrawlFile
	})
}
